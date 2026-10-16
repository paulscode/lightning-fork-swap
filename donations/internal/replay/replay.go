// Package replay judges whether a donation transaction could also be valid
// on the SHA256 chain, where it would move the donor's coins there to our
// address, and whether that happened.
//
// A transaction is valid there only if every input's signatures lack
// SIGHASH_UNIFIED and every coin it spends exists, unspent, on the SHA256
// chain. Coins created before the fork height exist there until moved; a
// coin created after it exists there too when the transaction that made it
// was copied there (its whole ancestry valid on both chains back to coins
// from before the split), so its height alone settles nothing.
package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// On the SHA256 chain: "unspent", "spent:<txid>", "copyable" (not
	// there, but every transaction back to the split could be copied
	// there first), "" when not looked up
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

// MaxAncestors is the most earlier transactions a check reads while
// following coins created after the fork back toward the split; past it
// the check is left "unknown" rather than called protected.
const MaxAncestors = 60

// Where a coin stands with respect to the SHA256 chain
type reach int

const (
	reachUnknown reach = iota
	// It exists there (created there; spent or not is a separate question)
	reachThere
	// Not there, but the transactions that lead to it could all be copied
	// there, after which it would exist there unspent
	reachCopyable
	// It can never exist there
	reachNever
)

// walk follows a transaction's ancestry on the BLAKE2b chain to decide
// whether its coins exist, or could come to exist, on the SHA256 chain.
type walk struct {
	ctx             context.Context
	blake2b, sha256 Explorer
	txs             map[string]reachOf
	reads           int
}

type reachOf struct {
	reach  reach
	why    string
	height int64 // on the BLAKE2b chain; 0 when unconfirmed or unread
}

// tx decides for a whole transaction: whether its outputs exist there.
func (w *walk) tx(txid string) reachOf {
	if r, ok := w.txs[txid]; ok {
		return r
	}
	r := w.read(txid)
	w.txs[txid] = r
	return r
}

func (w *walk) read(txid string) reachOf {
	if w.reads >= MaxAncestors {
		return reachOf{reach: reachUnknown, why: "its ancestry is too long to follow automatically: look by hand"}
	}
	w.reads++
	tx, err := w.blake2b.Tx(w.ctx, txid)
	if err != nil || tx == nil {
		return reachOf{reach: reachUnknown, why: "the BLAKE2b-chain explorers did not answer"}
	}
	var height int64
	if tx.Status.Confirmed {
		height = tx.Status.BlockHeight
		if height < ForkHeight {
			return reachOf{reach: reachThere, why: "created before the fork", height: height}
		}
	}
	// After the fork: there if it was copied there. The whole transaction,
	// not its status: explorers answer a status for transactions they don't
	// know as unconfirmed
	there, err := w.sha256.Tx(w.ctx, txid)
	switch {
	case err == nil && there != nil:
		return reachOf{reach: reachThere, why: "copied to the SHA256 chain", height: height}
	case !errors.Is(err, esplora.ErrNotFound):
		return reachOf{reach: reachUnknown, why: "the SHA256-chain explorers did not answer", height: height}
	}
	// Not there: could it be copied? Signatures first, they cost nothing
	unknown := ""
	for _, vin := range tx.Vin {
		if vin.IsCoinbase || vin.Prevout == nil {
			return reachOf{reach: reachNever, why: "it descends from coins mined after the fork", height: height}
		}
		switch protectionOf(vin) {
		case sighash.Protected:
			return reachOf{reach: reachNever, why: "an earlier transaction is signed with SIGHASH_UNIFIED", height: height}
		case sighash.Unknown:
			unknown = "an earlier transaction's signatures could not be read"
		}
	}
	for _, vin := range tx.Vin {
		c, why := w.coin(vin.Txid, vin.Vout, txid)
		switch c {
		case reachNever:
			return reachOf{reach: reachNever, why: why, height: height}
		case reachUnknown:
			unknown = why
		}
	}
	if unknown != "" {
		return reachOf{reach: reachUnknown, why: unknown, height: height}
	}
	return reachOf{reach: reachCopyable, why: "every transaction back to coins from before the fork could be copied", height: height}
}

// coin decides for one coin, as spent by spender: there and unspent (or
// spent by the spender itself) is there; spent there by something else
// can never serve the spender.
func (w *walk) coin(txid string, vout uint32, spender string) (reach, string) {
	r := w.tx(txid)
	if r.reach != reachThere {
		return r.reach, r.why
	}
	spend, err := w.sha256.Outspend(w.ctx, txid, vout)
	switch {
	case err != nil:
		return reachUnknown, "the SHA256-chain explorers did not answer"
	case spend.Spent && spend.Txid != spender:
		return reachNever, "an earlier coin was spent on the SHA256 chain by another transaction"
	}
	return reachThere, r.why
}

func protectionOf(vin esplora.Vin) sighash.Protection {
	read, err := sighash.Read(sighash.Input{
		ScriptType: vin.Prevout.Type, Witness: vin.Witness,
		ScriptSig: vin.ScriptSig,
	})
	if err != nil {
		return sighash.Unknown
	}
	return read.Protection
}

// CheckSHA256 judges a transaction of the SHA256 chain the other way
// round: whether it could also be valid on the BLAKE2b chain. The same
// rules hold in both directions; the reason names the chains accordingly,
// and Input.Sha256 then tells where each coin stands on the BLAKE2b chain.
func CheckSHA256(ctx context.Context, txid string, sha256, blake2b Explorer) (Result, error) {
	result, err := Check(ctx, txid, sha256, blake2b)
	swap := strings.NewReplacer("SHA256", "BLAKE2b", "BLAKE2b", "SHA256")
	result.Reason = swap.Replace(result.Reason)
	return result, err
}

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

	// The donation's own signatures first: they cost nothing
	unknown := ""
	for _, vin := range tx.Vin {
		in := Input{Prevout: fmt.Sprintf("%s:%d", vin.Txid, vin.Vout)}
		if vin.IsCoinbase || vin.Prevout == nil {
			// New coins: they exist on this chain only
			in.ScriptType = "coinbase"
			result.Inputs = append(result.Inputs, in)
			result.Verdict = Protected
			result.Reason = "spends coins the SHA256 chain does not have"
			return result, nil
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
		result.Inputs = append(result.Inputs, in)
		switch read.Protection {
		case sighash.Protected:
			result.Verdict = Protected
			result.Reason = "signed with SIGHASH_UNIFIED"
			return result, nil
		case sighash.Unknown:
			unknown = "some inputs' signatures could not be read"
		}
	}

	// Then the coins it spends: one that can never exist on the SHA256
	// chain keeps the whole transaction off it
	w := &walk{ctx: ctx, blake2b: blake2b, sha256: sha256, txs: map[string]reachOf{}}
	moved, copyable := false, false
	for i, vin := range tx.Vin {
		r := w.tx(vin.Txid)
		in := &result.Inputs[i]
		in.PreFork = r.height > 0 && r.height < ForkHeight
		switch r.reach {
		case reachNever:
			result.Verdict = Protected
			result.Reason = "spends coins that can never exist on the SHA256 chain: " + r.why
			return result, nil
		case reachUnknown:
			unknown = r.why
			continue
		case reachCopyable:
			in.Sha256 = "copyable"
			copyable = true
			continue
		}
		spend, err := sha256.Outspend(ctx, vin.Txid, vin.Vout)
		switch {
		case err != nil:
			unknown = "the SHA256-chain explorers did not answer"
		case !spend.Spent:
			in.Sha256 = "unspent"
		case spend.Txid == txid:
			// Copied, perhaps not yet confirmed
			in.Sha256 = "spent:" + spend.Txid
		default:
			in.Sha256 = "spent:" + spend.Txid
			moved = true
		}
	}

	addresses := map[string]bool{}
	for _, in := range result.Inputs {
		if in.Address != "" && !addresses[in.Address] {
			addresses[in.Address] = true
			result.AtRiskAddresses = append(result.AtRiskAddresses, in.Address)
		}
	}

	// Already copied over?
	if !copyable {
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
	}

	switch {
	case moved:
		result.Verdict = TwinMoved
		result.Reason = "the coins on the SHA256 chain were moved by another transaction"
	case unknown != "":
		result.Verdict = Unknown
		result.Reason = unknown
	case copyable:
		result.Verdict = AtRisk
		result.Reason = "not replay-protected, and the transactions before it could be copied to the SHA256 chain first"
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
