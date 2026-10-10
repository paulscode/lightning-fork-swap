// Package replay judges whether a donation transaction could also be valid
// on the SHA256 chain, where it would move the donor's coins there to our
// address, and whether that happened.
//
// A transaction is valid there only if every input's signatures lack
// SIGHASH_UNIFIED and every coin it spends exists, unspent, on the SHA256
// chain: coins created before the fork height, not yet moved there.
package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/sighash"
)

// ForkHeight: coins created below it exist on both chains.
const ForkHeight = 961640

// Verdict is what a check found.
type Verdict string

const (
	// Pending: not checked yet.
	Pending Verdict = "pending"
	// Protected: it cannot be valid on the SHA256 chain.
	Protected Verdict = "protected"
	// AtRisk: anyone could copy it to the SHA256 chain.
	AtRisk Verdict = "at_risk"
	// TwinMoved: the coins on the SHA256 chain were spent by something
	// else, usually the donor; it can no longer be copied there.
	TwinMoved Verdict = "twin_moved"
	// Replayed: the same transaction is confirmed on the SHA256 chain.
	Replayed Verdict = "replayed"
	// Unknown: the inputs could not be read, or the explorers did not
	// answer; checked again.
	Unknown Verdict = "unknown"
)

// Final verdicts are not checked again.
func (v Verdict) Final() bool {
	return v == Protected || v == TwinMoved || v == Replayed
}

// Explorer is what a check reads from each chain.
type Explorer interface {
	Tx(ctx context.Context, txid string) (*esplora.Tx, error)
	TxStatus(ctx context.Context, txid string) (*esplora.Status, error)
	Outspend(ctx context.Context, txid string, vout uint32) (*esplora.Outspend, error)
}

// Input is the report on one input.
type Input struct {
	Prevout    string             `json:"prevout"`
	Address    string             `json:"address,omitempty"`
	Value      int64              `json:"value"`
	ScriptType string             `json:"scriptType"`
	HashTypes  []string           `json:"hashTypes"`
	Protection sighash.Protection `json:"protection"`
	// Whether the spent coin was created before the fork (exists on both)
	PreFork bool `json:"preFork"`
	// On the SHA256 chain: "unspent", "spent:<txid>", "" when not looked up
	Sha256 string `json:"sha256,omitempty"`
}

// Result is a check's outcome.
type Result struct {
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason"`
	Inputs  []Input `json:"inputs"`
	// The donor's addresses whose SHA256-chain coins are at risk, or were
	// moved to us
	AtRiskAddresses []string `json:"atRiskAddresses"`
	// When replayed: where on the SHA256 chain
	ReplayedHeight int64 `json:"replayedHeight,omitempty"`
}

// MaxInputs is the most inputs a check looks up (a few explorer requests
// each, again on every re-check); a donation with more is left "unknown"
// for a look by hand.
const MaxInputs = 100

// Check judges a donation transaction.
func Check(ctx context.Context, txid string, blake2b, sha256 Explorer) (Result, error) {
	tx, err := blake2b.Tx(ctx, txid)
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", txid, err)
	}

	result := Result{Verdict: Unknown}
	if len(tx.Vin) > MaxInputs {
		result.Reason = fmt.Sprintf("%d inputs, more than are checked automatically (%d): look by hand", len(tx.Vin), MaxInputs)
		return result, nil
	}
	postFork, protected, unknown := false, false, false
	for _, vin := range tx.Vin {
		if postFork || protected {
			// Settled: one such input keeps the whole transaction off the
			// SHA256 chain; the rest are not looked up
			break
		}
		in := Input{Prevout: fmt.Sprintf("%s:%d", vin.Txid, vin.Vout)}
		if vin.IsCoinbase || vin.Prevout == nil {
			// New coins: they exist on this chain only
			in.ScriptType = "coinbase"
			result.Inputs = append(result.Inputs, in)
			postFork = true
			continue
		}
		in.Address = vin.Prevout.Address
		in.Value = vin.Prevout.Value
		in.ScriptType = vin.Prevout.Type

		read, err := sighash.Read(sighash.Input{
			ScriptType: vin.Prevout.Type, Witness: vin.Witness,
			ScriptSig: vin.ScriptSig,
		})
		if err != nil {
			read.Protection = sighash.Unknown
		}
		in.Protection = read.Protection
		for _, t := range read.HashTypes {
			in.HashTypes = append(in.HashTypes, fmt.Sprintf("0x%02x", t))
		}

		status, err := blake2b.TxStatus(ctx, vin.Txid)
		if err != nil {
			return Result{}, fmt.Errorf("reading %s: %w", vin.Txid, err)
		}
		in.PreFork = status.Confirmed && status.BlockHeight < ForkHeight
		if !in.PreFork {
			postFork = true
		}
		switch read.Protection {
		case sighash.Protected:
			protected = true
		case sighash.Unknown:
			unknown = true
		}
		result.Inputs = append(result.Inputs, in)
	}

	switch {
	case postFork:
		result.Verdict = Protected
		result.Reason = "spends coins created after the fork, which the SHA256 chain does not have"
		return result, nil
	case protected:
		result.Verdict = Protected
		result.Reason = "signed with SIGHASH_UNIFIED"
		return result, nil
	}

	addresses := map[string]bool{}
	for _, in := range result.Inputs {
		if in.Address != "" && !addresses[in.Address] {
			addresses[in.Address] = true
			result.AtRiskAddresses = append(result.AtRiskAddresses, in.Address)
		}
	}

	// Already copied over?
	status, err := sha256.TxStatus(ctx, txid)
	switch {
	case err == nil && status.Confirmed:
		result.Verdict = Replayed
		result.ReplayedHeight = status.BlockHeight
		result.Reason = "the same transaction is confirmed on the SHA256 chain"
		return result, nil
	case err != nil && !errors.Is(err, esplora.ErrNotFound):
		result.Reason = "the SHA256-chain explorers did not answer"
		return result, nil
	}

	moved := false
	for i, in := range result.Inputs {
		prev, vout := splitPrevout(in.Prevout)
		spend, err := sha256.Outspend(ctx, prev, vout)
		if err != nil {
			result.Reason = "the SHA256-chain explorers did not answer"
			return result, nil
		}
		switch {
		case !spend.Spent:
			result.Inputs[i].Sha256 = "unspent"
		case spend.Txid == txid:
			// In a SHA256 mempool: copied, not yet confirmed
			result.Inputs[i].Sha256 = "spent:" + spend.Txid
		default:
			result.Inputs[i].Sha256 = "spent:" + spend.Txid
			moved = true
		}
	}

	switch {
	case moved:
		result.Verdict = TwinMoved
		result.Reason = "the coins on the SHA256 chain were moved by another transaction"
	case unknown:
		result.Verdict = Unknown
		result.Reason = "some inputs' signatures could not be read"
	default:
		result.Verdict = AtRisk
		result.Reason = "not replay-protected, and the coins still exist unspent on the SHA256 chain"
	}
	return result, nil
}

func splitPrevout(prevout string) (string, uint32) {
	var vout uint32
	_, _ = fmt.Sscanf(prevout[65:], "%d", &vout)
	return prevout[:64], vout
}

// NextCheck is when a transaction is checked again; zero when never.
// At risk: hourly for 30 days, then daily for a year. Unknown: hourly for
// 7 days, then daily for 30 days. Pending: in a minute.
func NextCheck(v Verdict, firstSeen, now time.Time) time.Time {
	age := now.Sub(firstSeen)
	switch v {
	case Pending:
		return now.Add(time.Minute)
	case AtRisk:
		switch {
		case age < 30*24*time.Hour:
			return now.Add(time.Hour)
		case age < 365*24*time.Hour:
			return now.Add(24 * time.Hour)
		}
	case Unknown:
		switch {
		case age < 7*24*time.Hour:
			return now.Add(time.Hour)
		case age < 30*24*time.Hour:
			return now.Add(24 * time.Hour)
		}
	}
	return time.Time{}
}
