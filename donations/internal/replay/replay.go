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
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

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
	TxHex(ctx context.Context, txid string) (string, error)
	TxStatus(ctx context.Context, txid string) (*esplora.Status, error)
	Outspend(ctx context.Context, txid string, vout uint32) (*esplora.Outspend, error)
	TipHeight(ctx context.Context) (int64, error)
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

// SpendDepth: a coin spent on the other chain by another transaction counts
// as moved there only once that spend is this deep; until then it could be
// replaced or reorganised away, and the coin is still at risk.
const SpendDepth = 6

// Script flags of each chain's consensus (not relay policy: a legacy
// signature with an unusual hash type is non-standard on the SHA256 chain,
// yet valid in a block there). The BLAKE2b chain's adds the unified
// signature hash; that is the only difference that matters here.
const (
	sha256Consensus = txscript.ScriptBip16 | txscript.ScriptVerifyDERSignatures |
		txscript.ScriptVerifyCheckLockTimeVerify | txscript.ScriptVerifyCheckSequenceVerify |
		txscript.ScriptVerifyWitness | txscript.ScriptStrictMultiSig | txscript.ScriptVerifyTaproot
	blake2bConsensus = sha256Consensus | txscript.ScriptVerifyUnifiedSigHash
)

// direction: the rules of the chain a transaction is on (here) and of the
// one it might be copied to (there)
type direction struct{ here, there txscript.ScriptFlags }

var (
	toSHA256  = direction{here: blake2bConsensus, there: sha256Consensus}
	toBLAKE2b = direction{here: sha256Consensus, there: blake2bConsensus}
)

// Where a coin stands with respect to the other chain
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

// walk follows a transaction's ancestry on its chain (here) to decide
// whether its coins exist, or could come to exist, on the other (there).
type walk struct {
	ctx         context.Context
	here, there Explorer
	dir         direction
	txs         map[string]reachOf
	reads       int
	thereTip    int64
}

type reachOf struct {
	reach  reach
	why    string
	height int64 // here; 0 when unconfirmed or unread
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
	tx, err := w.here.Tx(w.ctx, txid)
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
	there, err := w.there.Tx(w.ctx, txid)
	switch {
	case err == nil && there != nil:
		return reachOf{reach: reachThere, why: "copied to the SHA256 chain", height: height}
	case !errors.Is(err, esplora.ErrNotFound):
		return reachOf{reach: reachUnknown, why: "the SHA256-chain explorers did not answer", height: height}
	}
	// Not there: could it be copied? New coins first, then its signatures
	for _, vin := range tx.Vin {
		if vin.IsCoinbase {
			return reachOf{reach: reachNever, why: "it descends from coins mined after the fork", height: height}
		}
	}
	unknown := ""
	verdicts, why := signedVerdicts(w.ctx, w.here, tx, w.dir)
	for _, v := range verdicts {
		switch v {
		case sighash.Protected:
			return reachOf{reach: reachNever, why: "an earlier transaction is signed so that the SHA256 chain refuses it", height: height}
		case sighash.Unknown:
			unknown = why
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
// spent by the spender itself, or by another transaction not yet deep) is
// there; spent there deep enough by something else can never serve the
// spender.
func (w *walk) coin(txid string, vout uint32, spender string) (reach, string) {
	r := w.tx(txid)
	if r.reach != reachThere {
		return r.reach, r.why
	}
	moved, err := w.movedThere(txid, vout, spender)
	switch {
	case err != nil:
		return reachUnknown, "the SHA256-chain explorers did not answer"
	case moved:
		return reachNever, "an earlier coin was spent on the SHA256 chain by another transaction"
	}
	return reachThere, r.why
}

// movedThere: the coin is spent on the other chain by a transaction other
// than spender, at least SpendDepth deep.
func (w *walk) movedThere(txid string, vout uint32, spender string) (bool, error) {
	spend, err := w.there.Outspend(w.ctx, txid, vout)
	if err != nil {
		return false, err
	}
	if !spend.Spent || spend.Txid == spender || spend.Status == nil ||
		!spend.Status.Confirmed || spend.Status.BlockHeight <= 0 {
		return false, nil
	}
	if w.thereTip == 0 {
		if w.thereTip, err = w.there.TipHeight(w.ctx); err != nil {
			return false, err
		}
	}
	return w.thereTip-spend.Status.BlockHeight+1 >= SpendDepth, nil
}

// signedVerdicts judges each input of tx as it is signed, by running its
// scripts: Protected when valid under the rules here and invalid under the
// rules there (only whoever holds its keys could sign it again for there),
// Unprotected when valid under both, Unknown when it cannot be verified
// (raw bytes or a spent output missing, or inconsistent). The raw bytes
// are checked against the txid; the spent outputs come from the explorer,
// and signatures commit to them, so wrong ones fail verification.
func signedVerdicts(ctx context.Context, here Explorer, tx *esplora.Tx, dir direction) ([]sighash.Protection, string) {
	unknown := func(why string) ([]sighash.Protection, string) {
		out := make([]sighash.Protection, len(tx.Vin))
		for i := range out {
			out[i] = sighash.Unknown
		}
		return out, why
	}
	raw, err := here.TxHex(ctx, tx.Txid)
	if err != nil {
		return unknown("a transaction's bytes could not be read")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return unknown("a transaction's bytes could not be read")
	}
	msg := wire.NewMsgTx(2)
	if err := msg.Deserialize(bytes.NewReader(b)); err != nil ||
		msg.TxHash().String() != tx.Txid || len(msg.TxIn) != len(tx.Vin) {
		return unknown("a transaction's bytes do not match it")
	}
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, vin := range tx.Vin {
		in := msg.TxIn[i].PreviousOutPoint
		if vin.Prevout == nil || in.Hash.String() != vin.Txid || in.Index != vin.Vout {
			return unknown("a spent output is missing")
		}
		script, err := hex.DecodeString(vin.Prevout.ScriptPubKey)
		if err != nil {
			return unknown("a spent output is missing")
		}
		fetcher.AddPrevOut(in, wire.NewTxOut(vin.Prevout.Value, script))
	}
	sigHashes := txscript.NewTxSigHashes(msg, fetcher)
	out := make([]sighash.Protection, len(msg.TxIn))
	why := ""
	for i := range msg.TxIn {
		prev := fetcher.FetchPrevOutput(msg.TxIn[i].PreviousOutPoint)
		valid := func(flags txscript.ScriptFlags) bool {
			vm, err := txscript.NewEngine(prev.PkScript, msg, i, flags, nil, sigHashes, prev.Value, fetcher)
			return err == nil && vm.Execute() == nil
		}
		switch {
		case !valid(dir.here):
			out[i], why = sighash.Unknown, "an input's signatures do not verify"
		case valid(dir.there):
			out[i] = sighash.Unprotected
		default:
			out[i] = sighash.Protected
		}
	}
	return out, why
}

// CheckSHA256 judges a transaction of the SHA256 chain the other way
// round: whether it could also be valid on the BLAKE2b chain. The same
// rules hold in both directions; the reason names the chains accordingly,
// and Input.Sha256 then tells where each coin stands on the BLAKE2b chain.
func CheckSHA256(ctx context.Context, txid string, sha256, blake2b Explorer) (Result, error) {
	result, err := check(ctx, txid, sha256, blake2b, toBLAKE2b)
	swap := strings.NewReplacer("SHA256", "BLAKE2b", "BLAKE2b", "SHA256")
	result.Reason = swap.Replace(result.Reason)
	return result, err
}

// Check judges a donation transaction of the BLAKE2b chain.
func Check(ctx context.Context, txid string, blake2b, sha256 Explorer) (Result, error) {
	return check(ctx, txid, blake2b, sha256, toSHA256)
}

func check(ctx context.Context, txid string, here, there Explorer, dir direction) (Result, error) {
	tx, err := here.Tx(ctx, txid)
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", txid, err)
	}

	result := Result{Verdict: Unknown}
	if len(tx.Vin) > MaxInputs {
		result.Reason = fmt.Sprintf("%d inputs, more than are checked automatically (%d): look by hand", len(tx.Vin), MaxInputs)
		return result, nil
	}

	// What the inputs are, then how they are signed
	unknown := ""
	for _, vin := range tx.Vin {
		in := Input{Prevout: fmt.Sprintf("%s:%d", vin.Txid, vin.Vout)}
		if vin.IsCoinbase {
			// New coins: they exist on this chain only
			in.ScriptType = "coinbase"
			result.Inputs = append(result.Inputs, in)
			result.Verdict = Protected
			result.Reason = "spends coins the SHA256 chain does not have"
			return result, nil
		}
		if vin.Prevout == nil {
			result.Inputs = append(result.Inputs, in)
			unknown = "a spent output is missing"
			continue
		}
		in.Address = vin.Prevout.Address
		in.Value = vin.Prevout.Value
		in.ScriptType = vin.Prevout.Type
		// The hash types, for the report; the verdict comes from the scripts
		if read, err := sighash.Read(sighash.Input{ScriptType: vin.Prevout.Type,
			Witness: vin.Witness, ScriptSig: vin.ScriptSig}); err == nil {
			for _, t := range read.HashTypes {
				in.HashTypes = append(in.HashTypes, fmt.Sprintf("0x%02x", t))
			}
		}
		result.Inputs = append(result.Inputs, in)
	}
	verdicts, why := signedVerdicts(ctx, here, tx, dir)
	for i, v := range verdicts {
		result.Inputs[i].Protection = v
		switch v {
		case sighash.Protected:
			result.Verdict = Protected
			result.Reason = "an input, as signed, is invalid on the SHA256 chain (signed with SIGHASH_UNIFIED)"
			return result, nil
		case sighash.Unknown:
			unknown = why
		}
	}

	// Then the coins it spends: one that can never exist on the other
	// chain keeps the whole transaction off it
	w := &walk{ctx: ctx, here: here, there: there, dir: dir, txs: map[string]reachOf{}}
	moved, copyable := false, false
	for i, vin := range tx.Vin {
		if vin.Prevout == nil {
			continue
		}
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
		spend, err := there.Outspend(ctx, vin.Txid, vin.Vout)
		if err != nil {
			unknown = "the SHA256-chain explorers did not answer"
			continue
		}
		deep, err := w.movedThere(vin.Txid, vin.Vout, txid)
		switch {
		case err != nil:
			unknown = "the SHA256-chain explorers did not answer"
		case !spend.Spent:
			in.Sha256 = "unspent"
		case spend.Txid == txid:
			// Copied, perhaps not yet confirmed
			in.Sha256 = "spent:" + spend.Txid
		case deep:
			in.Sha256 = "spent:" + spend.Txid
			moved = true
		default:
			// Spent there by something else, not deep yet: it may still
			// be replaced or reorganised away
			in.Sha256 = "spent (not yet deep):" + spend.Txid
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
		status, err := there.TxStatus(ctx, txid)
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
		result.Reason = "the coins on the SHA256 chain were moved by another transaction, at least 6 blocks deep"
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
