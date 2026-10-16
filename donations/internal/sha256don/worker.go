package sha256don

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/paulscode/lightning-fork-swap/donations/internal/bip322"
	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/knots"
	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
)

// Explorer is what the worker reads from a chain's explorer, and relays
// through (the SHA256 chain's: the service runs no node of it).
type Explorer interface {
	AddressTxs(ctx context.Context, address string) ([]esplora.Tx, error)
	Tx(ctx context.Context, txid string) (*esplora.Tx, error)
	TxHex(ctx context.Context, txid string) (string, error)
	TxStatus(ctx context.Context, txid string) (*esplora.Status, error)
	Outspend(ctx context.Context, txid string, vout uint32) (*esplora.Outspend, error)
	TipHeight(ctx context.Context) (int64, error)
	FeeRate(ctx context.Context, blocks int) (float64, error)
	Broadcast(ctx context.Context, raw string) (string, error)
}

// Node is our BLAKE2b-chain node: the truth for everything we act on.
type Node interface {
	TxOut(ctx context.Context, txid string, vout uint32, mempool bool) (*knots.TxOut, error)
	TestAccept(ctx context.Context, raw string) (knots.Accept, error)
	Send(ctx context.Context, raw string) (string, error)
	FeeRate(ctx context.Context, blocks int) (float64, error)
	Height(ctx context.Context) (int64, error)
}

// Worker runs SHA256-chain donations.
type Worker struct {
	Store   Store
	Keys    *Keys
	Params  *chaincfg.Params
	Sha256  Explorer
	Blake2b Explorer
	Node    Node
	// Where BLAKE2b coins of requests without a return address go: an
	// address of the swap service's lnd wallet
	LndAddress string
	Now        func() time.Time
	Logger     *log.Logger

	// A SHA256 deposit counts at this depth; a BLAKE2b one is passed on
	// at this depth, and a forward is done at it
	Sha256Confs, Blake2bConfs int64
	// Fee estimate target (blocks), and the most paid (sat/vB)
	FeeBlocks  int
	MaxFeeRate float64
	// The most requests looked at in one pass
	MaxPolls int
	// Checks a proof; nil is BIP322
	Verify func(address, message, signature string) error

	// Sweeps: the SHA256-chain donations sent on to SweepAddress (the
	// operator's) every SweepEvery, when they come to SweepMinSat or more;
	// "" never. SweepMaxFee caps the rate (sat/vB); a sweep is done at
	// Sha256Confs.
	SweepAddress string
	SweepMinSat  int64
	SweepEvery   time.Duration
	// The most paid on the SHA256 chain (locks and sweeps), sat/vB
	SweepMaxFee float64
	// When the BLAKE2b chain's data rules (RDTS) end; locks are made only
	// until RDTSMargin before it. Zero: RDTSExpiry.
	RDTSUntil time.Time

	// A lock counts at this depth on the SHA256 chain; before a forward,
	// every one of Verifiers (each a single SHA256 explorer; empty: Sha256)
	// must agree that the twin is spent by the lock this deep
	LockConfs int64
	Verifiers []Explorer
	// Outputs under MinDepositSat are kept, never locked nor passed on; a
	// request takes at most MaxDeposits; a transaction spends at most
	// MaxTxInputs
	MinDepositSat int64
	MaxDeposits   int
	MaxTxInputs   int

	checks      map[string]replay.Result // this pass's
	failedAlone map[string]time.Time     // outpoints whose forward failed in a batch
}

// RDTSExpiry is when the BLAKE2b chain's data rules end on mainnet
// (getdeploymentinfo: reduced_data expiry_time). A lock is refused there
// only while they hold.
var RDTSExpiry = time.Unix(1819756800, 0)

// RDTSMargin: no new lock this close to the end of the data rules.
const RDTSMargin = 30 * 24 * time.Hour

// Lock and limit defaults
const (
	DefaultLockConfs     = 6
	DefaultMinDepositSat = 5_000
	DefaultMaxDeposits   = 50
	DefaultMaxTxInputs   = 100
)

// Sweep defaults
const (
	DefaultSweepMinSat = 50_000
	DefaultSweepEvery  = 24 * time.Hour
	DefaultSweepMaxFee = 50
)

func (w *Worker) verify(address, message, signature string) error {
	if w.Verify != nil {
		return w.Verify(address, message, signature)
	}
	return bip322.Verify(address, message, signature, w.Params)
}

// Defaults
const (
	DefaultSha256Confs  = 3
	DefaultBlake2bConfs = 6
	DefaultFeeBlocks    = 6
	DefaultMaxFeeRate   = 100
	DefaultMaxPolls     = 40
	// A deposit missing this many looks in a row is gone
	missingLimit = 3
	fallbackFee  = 2
)

func (w *Worker) defaults() {
	if w.Sha256Confs == 0 {
		w.Sha256Confs = DefaultSha256Confs
	}
	if w.Blake2bConfs == 0 {
		w.Blake2bConfs = DefaultBlake2bConfs
	}
	if w.FeeBlocks == 0 {
		w.FeeBlocks = DefaultFeeBlocks
	}
	if w.MaxFeeRate == 0 {
		w.MaxFeeRate = DefaultMaxFeeRate
	}
	if w.MaxPolls == 0 {
		w.MaxPolls = DefaultMaxPolls
	}
	if w.SweepMinSat == 0 {
		w.SweepMinSat = DefaultSweepMinSat
	}
	if w.SweepEvery == 0 {
		w.SweepEvery = DefaultSweepEvery
	}
	if w.SweepMaxFee == 0 {
		w.SweepMaxFee = DefaultSweepMaxFee
	}
	if w.RDTSUntil.IsZero() {
		w.RDTSUntil = RDTSExpiry
	}
	if w.LockConfs == 0 {
		w.LockConfs = DefaultLockConfs
	}
	if len(w.Verifiers) == 0 {
		w.Verifiers = []Explorer{w.Sha256}
	}
	if w.MinDepositSat == 0 {
		w.MinDepositSat = DefaultMinDepositSat
	}
	if w.MaxDeposits == 0 {
		w.MaxDeposits = DefaultMaxDeposits
	}
	if w.MaxTxInputs == 0 {
		w.MaxTxInputs = DefaultMaxTxInputs
	}
	if w.failedAlone == nil {
		w.failedAlone = map[string]time.Time{}
	}
	if w.Logger == nil {
		w.Logger = log.Default()
	}
}

// Run passes every interval, and at once when woken.
func (w *Worker) Run(ctx context.Context, interval time.Duration, wake <-chan struct{}) {
	for {
		if err := w.Pass(ctx); err != nil && ctx.Err() == nil {
			w.Logger.Printf("sha256 donations: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-wake:
		}
	}
}

// PollEvery is how often a request is looked at, by age: often while new,
// rarely after a month, not after a year.
func PollEvery(age time.Duration) time.Duration {
	switch {
	case age < 24*time.Hour:
		return 0 // every pass
	case age < 7*24*time.Hour:
		return 10 * time.Minute
	case age < 30*24*time.Hour:
		return time.Hour
	case age < 365*24*time.Hour:
		return 24 * time.Hour
	}
	return -1
}

// Pass does one round.
func (w *Worker) Pass(ctx context.Context) error {
	w.defaults()
	now := w.Now()
	if err := w.assign(ctx); err != nil {
		return fmt.Errorf("assigning addresses: %w", err)
	}
	var errs []error
	w.checks = map[string]replay.Result{}
	requests, err := w.Store.Due(ctx, now, w.MaxPolls)
	if err != nil {
		return err
	}
	var shaTip int64
	for _, r := range requests {
		if ctx.Err() != nil {
			break
		}
		if shaTip == 0 {
			if shaTip, err = w.Sha256.TipHeight(ctx); err != nil {
				errs = append(errs, fmt.Errorf("SHA256 tip: %w", err))
				break
			}
		}
		if err := w.poll(ctx, r, shaTip); err != nil {
			errs = append(errs, fmt.Errorf("request %s: %w", r.ID, err))
		}
		// Looked at, even when it failed: a request whose look keeps
		// failing must not keep the others from theirs
		if err := w.Store.Polled(ctx, r.ID, now); err != nil {
			errs = append(errs, err)
		}
	}
	if err := w.lock(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("locking: %w", err))
	}
	if err := w.trackLocks(ctx); err != nil {
		errs = append(errs, fmt.Errorf("following locks: %w", err))
	}
	if err := w.permanence(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("permanence: %w", err))
	}
	if err := w.forward(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("forwarding: %w", err))
	}
	if err := w.track(ctx); err != nil {
		errs = append(errs, fmt.Errorf("following forwards: %w", err))
	}
	if err := w.sweep(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("sweeping: %w", err))
	}
	if err := w.trackSweeps(ctx); err != nil {
		errs = append(errs, fmt.Errorf("following sweeps: %w", err))
	}
	// Alive and handing out addresses: what the API and the monitor ask.
	// A request whose explorer failed is looked at again next pass.
	_ = w.Store.SetSetting(ctx, SettingLastPass, now.UTC().Format(time.RFC3339))
	return errors.Join(errs...)
}

func (w *Worker) assign(ctx context.Context) error {
	reqs, err := w.Store.Unassigned(ctx)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if err := w.Store.Assign(ctx, r.ID, func(i int64) (string, error) {
			if i < 0 || i >= 1<<31 {
				return "", errors.New("out of addresses")
			}
			addr, _, err := w.Keys.Address(uint32(i))
			return addr, err
		}); err != nil {
			return err
		}
	}
	return nil
}

// poll looks for a request's deposits on both chains, and copies when
// asked.
func (w *Worker) poll(ctx context.Context, r Request, shaTip int64) error {
	known, err := w.Store.Deposits(ctx, r.ID)
	if err != nil {
		return err
	}

	// The SHA256 chain: what pays the address
	txs, err := w.Sha256.AddressTxs(ctx, r.Address)
	if err != nil {
		return fmt.Errorf("SHA256 explorer: %w", err)
	}
	seenSha := map[string]bool{}
	for _, tx := range txs {
		for i, out := range tx.Vout {
			if out.Address != r.Address || out.Value <= 0 {
				continue
			}
			op := fmt.Sprintf("%s:%d", tx.Txid, i)
			if !w.room(known, SHA256, op) {
				continue
			}
			confs := int64(0)
			if tx.Status.Confirmed && tx.Status.BlockHeight > 0 {
				confs = shaTip - tx.Status.BlockHeight + 1
			}
			state := Seen
			if out.Value < w.MinDepositSat {
				state = Kept // dust: never locked
			}
			d, err := w.Store.PutDeposit(ctx, Deposit{RequestID: r.ID, Chain: SHA256,
				Txid: tx.Txid, Vout: uint32(i), ValueSat: out.Value,
				Confirmations: confs, State: state})
			if err != nil {
				return err
			}
			known = append(known, d)
			seenSha[d.Outpoint()] = true
			if d.State == Seen && confs >= w.Sha256Confs {
				d.State = Confirmed
				if err := w.Store.SaveDeposit(ctx, d); err != nil {
					return err
				}
				w.Logger.Printf("SHA256 donation %s: %d sat (request %s)", d.Outpoint(), d.ValueSat, r.ID)
			}
		}
	}
	for _, d := range known {
		if d.Chain == SHA256 && d.State == Seen && !seenSha[d.Outpoint()] {
			// Not in the listing (which a flood of payments can push it
			// out of): gone only if the transaction itself is gone
			tx, err := w.Sha256.Tx(ctx, d.Txid)
			if err == nil && tx != nil {
				if tx.Status.Confirmed && tx.Status.BlockHeight > 0 {
					d.Confirmations = shaTip - tx.Status.BlockHeight + 1
					if d.Confirmations >= w.Sha256Confs {
						d.State = Confirmed
					}
				}
				if err := w.Store.SaveDeposit(ctx, d); err != nil {
					return err
				}
				continue
			}
			if !errors.Is(err, esplora.ErrNotFound) {
				continue
			}
			// An unconfirmed payment that left: replaced, or dropped
			d.Missing++
			if d.Missing >= missingLimit {
				d.State = Gone
			}
			if err := w.Store.SaveDeposit(ctx, d); err != nil {
				return err
			}
		}
	}

	// The BLAKE2b chain: what the explorer lists for the address, and the
	// SHA256 deposits' own outputs (copies, usually there by themselves),
	// each checked on our node
	candidates := map[string][2]any{}
	add := func(txid string, vout uint32) {
		candidates[fmt.Sprintf("%s:%d", txid, vout)] = [2]any{txid, vout}
	}
	if btxs, err := w.Blake2b.AddressTxs(ctx, r.Address); err == nil {
		for _, tx := range btxs {
			for i, out := range tx.Vout {
				if out.Address == r.Address {
					add(tx.Txid, uint32(i))
				}
			}
		}
	} else {
		w.Logger.Printf("BLAKE2b explorer, request %s: %v", r.ID, err)
	}
	shaDeposits, err := w.Store.Deposits(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, d := range shaDeposits {
		if d.Chain == SHA256 && d.State != Gone {
			add(d.Txid, d.Vout)
		}
	}
	for _, d := range shaDeposits {
		if d.Chain == BLAKE2b && (d.State == Seen || d.State == Ready || d.State == Gone) {
			add(d.Txid, d.Vout) // gone ones too: they come back if they reappear
		}
	}
	_, script, err := w.Keys.Address(uint32(r.KeyIndex))
	if err != nil {
		return err
	}
	ours := hex.EncodeToString(script)
	found := map[string]bool{}
	keys := make([]string, 0, len(candidates))
	for k := range candidates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := candidates[k]
		txid, vout := c[0].(string), c[1].(uint32)
		out, err := w.Node.TxOut(ctx, txid, vout, true)
		if err != nil {
			return fmt.Errorf("our node: %w", err)
		}
		if out == nil || out.ScriptPubKey != ours || out.ValueSat <= 0 {
			continue
		}
		if !w.room(shaDeposits, BLAKE2b, k) {
			continue
		}
		found[k] = true
		state := Seen
		if out.ValueSat < w.MinDepositSat {
			state = Kept // dust: never passed on
		}
		d, err := w.Store.PutDeposit(ctx, Deposit{RequestID: r.ID, Chain: BLAKE2b,
			Txid: txid, Vout: vout, ValueSat: out.ValueSat,
			Confirmations: out.Confirmations, State: state})
		if err != nil {
			return err
		}
		shaDeposits = append(shaDeposits, d)
		if d.State == Seen && out.Confirmations >= w.Blake2bConfs {
			d.State = Ready
			if err := w.Store.SaveDeposit(ctx, d); err != nil {
				return err
			}
		}
	}
	after, err := w.Store.Deposits(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, d := range after {
		if d.Chain != BLAKE2b || found[d.Outpoint()] || (d.State != Seen && d.State != Ready) {
			continue
		}
		// Not in the UTXO set nor the mempool, and not spent by us
		d.Missing++
		if d.Missing >= missingLimit {
			if d.State == Ready {
				w.Logger.Printf("ALERT: BLAKE2b deposit %s of request %s left the chain after %d confirmations",
					d.Outpoint(), r.ID, d.Confirmations)
			}
			d.State = Gone
		}
		if err := w.Store.SaveDeposit(ctx, d); err != nil {
			return err
		}
	}

	if r.Replay && r.ReturnAddress != "" {
		return w.copies(ctx, r, after)
	}
	return nil
}

// room: a new deposit fits under the request's cap (one already known
// always does).
func (w *Worker) room(known []Deposit, chain, outpoint string) bool {
	n := 0
	for _, d := range known {
		if d.Chain != chain {
			continue
		}
		if d.Outpoint() == outpoint {
			return true
		}
		if d.State != Gone {
			n++
		}
	}
	return n < w.MaxDeposits
}

// copies relays the request's SHA256 donations to the BLAKE2b chain, those
// that aren't there yet and whose inputs' owners asked for it. Only the
// donation itself: copying an earlier transaction would move coins of
// whoever made it.
func (w *Worker) copies(ctx context.Context, r Request, deposits []Deposit) error {
	there := map[string]bool{}
	for _, d := range deposits {
		if d.Chain == BLAKE2b {
			there[d.Outpoint()] = true
		}
	}
	for _, d := range deposits {
		if d.Chain != SHA256 || d.State == Gone || d.Confirmations < 1 {
			continue
		}
		switch d.CopyState {
		case CopyAlready, CopyDone:
			continue
		}
		before := d
		if there[d.Outpoint()] {
			d.CopyState, d.CopyReason = CopyAlready, ""
		} else {
			state, reason, err := w.copy(ctx, r, d)
			if err != nil {
				return err
			}
			d.CopyState, d.CopyReason = state, reason
		}
		if d.CopyState != before.CopyState || d.CopyReason != before.CopyReason {
			if d.CopyState == CopyDone {
				w.Logger.Printf("copied %s to the BLAKE2b chain (request %s)", d.Txid, r.ID)
			}
			if err := w.Store.SaveDeposit(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Worker) copy(ctx context.Context, r Request, d Deposit) (string, string, error) {
	tx, err := w.Sha256.Tx(ctx, d.Txid)
	if err != nil {
		return d.CopyState, d.CopyReason, nil // try again next time
	}
	// Every input's address must have asked, through a valid proof
	hints, err := w.Store.Hints(ctx, r.ID, d.Txid)
	if err != nil {
		return "", "", err
	}
	message := ProofMessage(r.ID, d.Txid, r.ReturnAddress)
	proven := map[string]bool{}
	for _, h := range hints {
		for _, p := range h.Proofs {
			if w.verify(p.Address, message, p.Signature) == nil {
				proven[p.Address] = true
			}
		}
	}
	var missing []string
	for _, in := range tx.Vin {
		if in.Prevout == nil || in.Prevout.Address == "" {
			return CopyNotPossible, "an input has no address to prove", nil
		}
		if !proven[in.Prevout.Address] {
			missing = append(missing, in.Prevout.Address)
		}
	}
	if len(missing) > 0 {
		return CopyNoProof, "proofs needed from " + strings.Join(dedupe(missing), ", "), nil
	}

	raw, err := w.Sha256.TxHex(ctx, d.Txid)
	if err != nil {
		return d.CopyState, d.CopyReason, nil
	}
	// The explorer's bytes must be the transaction asked about, and its
	// claimed spent outputs (whose addresses the proofs are for) the ones
	// its signatures commit to
	if got, err := txidOf(raw); err != nil || got != d.Txid {
		return d.CopyState, d.CopyReason, nil
	}
	if err := verifyInputs(raw, tx); err != nil {
		return CopyNotPossible, "its inputs could not be verified: " + err.Error(), nil
	}
	acc, err := w.Node.TestAccept(ctx, raw)
	if err != nil {
		return "", "", fmt.Errorf("our node: %w", err)
	}
	if !acc.Allowed {
		if strings.Contains(acc.Reason, "already") || strings.Contains(acc.Reason, "known") {
			return CopyAlready, "", nil
		}
		return CopyNotPossible, explainRefusal(acc.Reason), nil
	}
	if _, err := w.Node.Send(ctx, raw); err != nil {
		return "", "", fmt.Errorf("relaying the copy: %w", err)
	}
	return CopyDone, "", nil
}

func explainRefusal(reason string) string {
	switch {
	case strings.Contains(reason, "missingorspent") || strings.Contains(reason, "missing-inputs"):
		return "its coins don't exist unspent on the BLAKE2b chain: spent differently there, or made by transactions that never reached it (only the donation itself is copied, never earlier transactions)"
	case strings.Contains(reason, "script-verify"):
		return "the BLAKE2b chain refuses its signatures"
	}
	return "the BLAKE2b chain refuses it: " + reason
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func txidOf(raw string) (string, error) {
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", err
	}
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		return "", err
	}
	return tx.TxHash().String(), nil
}

// forward passes deep BLAKE2b deposits on: each request's to its return
// address, the rest together to lnd.
func (w *Worker) forward(ctx context.Context, now time.Time) error {
	all, err := w.Store.DepositsIn(ctx, BLAKE2b, Ready)
	if err != nil || len(all) == 0 {
		return err
	}
	var ready []Deposit
	for _, d := range all {
		if at, ok := w.failedAlone[d.Outpoint()]; ok && now.Sub(at) < time.Hour {
			continue // its batch was refused: alone, in an hour
		}
		ok, d, err := w.cleared(ctx, d, now)
		if err != nil {
			return err
		}
		if ok {
			ready = append(ready, d)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	type group struct {
		requestID, dest string
		coins           []Coin
		ids             []int64
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range ready {
		r, err := w.Store.GetRequest(ctx, d.RequestID)
		if err != nil {
			return err
		}
		key, dest, rid := r.ID, r.ReturnAddress, r.ID
		if dest == "" {
			key, dest, rid = "lnd", w.LndAddress, ""
		}
		if dest == "" || dest == r.Address {
			continue
		}
		if _, alone := w.failedAlone[d.Outpoint()]; alone {
			key = "alone:" + d.Outpoint()
		}
		if g, ok := groups[key]; ok && len(g.coins) >= w.MaxTxInputs {
			continue // next pass
		}
		g, ok := groups[key]
		if !ok {
			g = &group{requestID: rid, dest: dest}
			groups[key] = g
			order = append(order, key)
		}
		g.coins = append(g.coins, Coin{Txid: d.Txid, Vout: d.Vout, ValueSat: d.ValueSat,
			KeyIndex: uint32(r.KeyIndex)})
		g.ids = append(g.ids, d.ID)
	}
	if len(order) == 0 {
		return nil
	}
	rate, err := w.Node.FeeRate(ctx, w.FeeBlocks)
	if err != nil || rate <= 0 {
		rate = fallbackFee
	}
	if rate > w.MaxFeeRate {
		rate = w.MaxFeeRate
	}
	var errs []error
	for _, key := range order {
		g := groups[key]
		s, err := Sign(w.Keys, g.coins, g.dest, rate, w.Params)
		if errors.Is(err, ErrTooSmall) {
			continue // more may come; until then it waits
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		f := Forward{RequestID: g.requestID, Txid: s.Txid, Raw: s.Raw, Dest: g.dest,
			AmountSat: s.AmountSat, FeeSat: s.FeeSat, State: Sent}
		// Recorded before it is relayed: its coins are never signed twice
		id, err := w.Store.CreateForward(ctx, f, g.ids)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		f.ID = id
		acc, err := w.Node.TestAccept(ctx, s.Raw)
		if err != nil {
			errs = append(errs, err) // relayed on a later pass
			continue
		}
		if !acc.Allowed {
			// Never relayed: its coins go back to Ready, each to be tried
			// alone, so one bad coin does not hold the others
			f.State, f.Error = FailedSend, acc.Reason
			for _, c := range g.coins {
				w.failedAlone[c.Outpoint()] = now
			}
			if len(g.coins) == 1 {
				w.alertOnce(ctx, now, "forward:"+g.coins[0].Outpoint(),
					fmt.Sprintf("forward of %s refused by our node: %s", g.coins[0].Outpoint(), acc.Reason))
			}
			errs = append(errs, w.Store.EndForward(ctx, f))
			continue
		}
		for _, c := range g.coins {
			delete(w.failedAlone, c.Outpoint())
		}
		if _, err := w.Node.Send(ctx, s.Raw); err != nil {
			errs = append(errs, err)
			continue
		}
		w.Logger.Printf("passed on %d sat to %s in %s (fee %d)", s.AmountSat, g.dest, s.Txid, s.FeeSat)
	}
	return errors.Join(errs...)
}

// track follows forwards until they are deep enough, relaying again any
// that dropped out of the mempool.
func (w *Worker) track(ctx context.Context) error {
	sent, err := w.Store.ForwardsIn(ctx, Sent)
	if err != nil || len(sent) == 0 {
		return err
	}
	height, err := w.Node.Height(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range sent {
		confs := int64(-1)
		if out, err := w.Node.TxOut(ctx, f.Txid, 0, true); err != nil {
			errs = append(errs, err)
			continue
		} else if out != nil {
			confs = out.Confirmations
		} else if st, err := w.Blake2b.TxStatus(ctx, f.Txid); err == nil && st.Confirmed && st.BlockHeight > 0 {
			// Already spent by its recipient
			confs = height - st.BlockHeight + 1
		}
		if confs < 0 {
			// Not in our node's UTXO set nor its mempool: an explorer that
			// has it (its output already spent) settles it; else relay again
			if _, err := w.Blake2b.Tx(ctx, f.Txid); err == nil {
				continue
			}
			if _, err := w.Node.Send(ctx, f.Raw); err != nil {
				f.Error = err.Error()
				w.Logger.Printf("ALERT: forward %s cannot be relayed again: %v", f.Txid, err)
				errs = append(errs, w.Store.SaveForward(ctx, f))
			}
			continue
		}
		f.Confirmations = confs
		if confs >= w.Blake2bConfs {
			f.State = Done
			errs = append(errs, w.Store.EndForward(ctx, f))
			continue
		}
		errs = append(errs, w.Store.SaveForward(ctx, f))
	}
	return errors.Join(errs...)
}

// cleared: the coin cannot be valid on the SHA256 chain, so nothing that
// spends it, however it is signed (by us too), can be copied there; anyone
// can check it. Either its twin there is spent by one of our locks, deep
// enough by every SHA256 explorer we use, or the transaction that made it
// cannot reach that chain as signed or by its history. Else it is held,
// and the reason (saved with the deposit, returned) says what for.
func (w *Worker) cleared(ctx context.Context, d Deposit, now time.Time) (bool, Deposit, error) {
	before := d.HoldChecked
	ok, hold, err := w.clearance(ctx, &d, now)
	if err != nil {
		return false, d, err
	}
	if !ok && hold == "" {
		hold = "held"
	}
	if ok {
		hold = ""
	}
	if d.Hold != hold || d.HoldChecked != before {
		d.Hold = hold
		if err := w.Store.SaveDeposit(ctx, d); err != nil {
			return false, d, err
		}
	}
	return ok, d, nil
}

func (w *Worker) clearance(ctx context.Context, d *Deposit, now time.Time) (bool, string, error) {
	deposits, err := w.Store.Deposits(ctx, d.RequestID)
	if err != nil {
		return false, "", err
	}
	for _, twin := range deposits {
		if twin.Chain != SHA256 || twin.Txid != d.Txid || twin.Vout != d.Vout || twin.State == Gone {
			continue
		}
		if twin.State != Locked {
			return false, "waiting for its twin on the SHA256 chain to be locked", nil
		}
		lock, err := w.Store.GetLock(ctx, twin.LockID)
		if err != nil {
			return false, "", err
		}
		if why := w.lockHolds(ctx, twin, lock); why != "" {
			return false, why, nil
		}
		return true, "", nil
	}
	// No twin of ours there: can there ever be one? Looked at again less
	// often as the deposit ages (10 minutes at first, a day at most)
	if d.Hold != "" && d.HoldChecked != nil {
		every := now.Sub(d.FirstSeen) / 6
		if every < 10*time.Minute {
			every = 10 * time.Minute
		}
		if every > 24*time.Hour {
			every = 24 * time.Hour
		}
		if now.Sub(*d.HoldChecked) < every {
			return false, d.Hold, nil
		}
	}
	result, ok := w.checks[d.Txid]
	if !ok {
		result, err = replay.Check(ctx, d.Txid, w.Blake2b, w.Sha256)
		if err != nil {
			result = replay.Result{Verdict: replay.Unknown, Reason: "its history could not be read yet"}
		}
		if w.checks != nil {
			w.checks[d.Txid] = result
		}
	}
	d.HoldChecked = &now
	switch result.Verdict {
	case replay.Protected, replay.TwinMoved:
		return true, "", nil
	case replay.Replayed:
		return false, "copied to the SHA256 chain: waiting for that copy to be locked", nil
	}
	return false, "its coins could still appear on the SHA256 chain (" + result.Reason +
		"): passed on once they cannot", nil
}

// lockHolds: every SHA256 explorer we use says the twin is spent by the
// lock, at least LockConfs deep. "" when it holds, else why not.
func (w *Worker) lockHolds(ctx context.Context, twin Deposit, lock Lock) string {
	for _, v := range w.Verifiers {
		spend, err := v.Outspend(ctx, twin.Txid, twin.Vout)
		if err != nil {
			return "its lock could not be confirmed with every SHA256 explorer"
		}
		if !spend.Spent || spend.Txid != lock.Txid {
			return "an explorer does not see its twin spent by its lock"
		}
		if spend.Status == nil || !spend.Status.Confirmed || spend.Status.BlockHeight <= 0 {
			return "its lock is not confirmed by every SHA256 explorer"
		}
		tip, err := v.TipHeight(ctx)
		if err != nil {
			return "its lock could not be confirmed with every SHA256 explorer"
		}
		if tip-spend.Status.BlockHeight+1 < w.LockConfs {
			return fmt.Sprintf("its lock is not yet %d deep by every SHA256 explorer", w.LockConfs)
		}
	}
	return ""
}

// alertOnce logs an alert at most once a day per key (the monitor sends
// each new line).
func (w *Worker) alertOnce(ctx context.Context, now time.Time, key, text string) {
	day := now.UTC().Format("2006-01-02")
	if got, _ := w.Store.Setting(ctx, "alert:"+key); got == day {
		return
	}
	_ = w.Store.SetSetting(ctx, "alert:"+key, day)
	w.Logger.Printf("ALERT: %s", text)
}

// lock spends confirmed SHA256-chain donations, on the SHA256 chain, into
// the hot wallet, with an output the BLAKE2b chain's consensus refuses
// (SignLock). Once a lock is deep, a donation's twin on the BLAKE2b chain
// can be passed on: what spends it there can never be valid on the SHA256
// chain, where its coin is already spent.
// RDTSWarning: from this long before the data rules end, an alert a day
// counts the locks not yet permanent: such a lock could, once the rules
// end, be mined on the BLAKE2b chain (if every coin it spends exists
// there), taking a twin to our sweep address instead of its donor.
const RDTSWarning = 90 * 24 * time.Hour

// permanence marks the locks that can never be valid on the BLAKE2b
// chain, whatever its data rules: one of their inputs can never exist
// unspent there. That is a deposit whose BLAKE2b twin we passed on (spent
// there, deep), or the anchor of a lock that is permanent itself; anchors
// chain the locks, so one permanent lock makes every later one permanent.
func (w *Worker) permanence(ctx context.Context, now time.Time) error {
	locks, err := w.Store.NotPermanent(ctx)
	if err != nil {
		return err
	}
	locked, err := w.Store.DepositsIn(ctx, SHA256, Locked)
	if err != nil {
		return err
	}
	permanent := map[int64]bool{}
	left := 0
	for _, l := range locks {
		ok := false
		if l.AnchorIn != 0 {
			if permanent[l.AnchorIn] {
				ok = true
			} else if in, err := w.Store.GetLock(ctx, l.AnchorIn); err == nil && in.Permanent {
				ok = true
			}
		}
		for _, d := range locked {
			if ok || d.LockID != l.ID {
				continue
			}
			all, err := w.Store.Deposits(ctx, d.RequestID)
			if err != nil {
				return err
			}
			for _, b := range all {
				if b.Chain == BLAKE2b && b.Txid == d.Txid && b.Vout == d.Vout && b.State == Forwarded {
					ok = true
				}
			}
		}
		if ok {
			permanent[l.ID] = true
			if err := w.Store.SetPermanent(ctx, l.ID); err != nil {
				return err
			}
			continue
		}
		left++
	}
	if left > 0 && !now.Before(w.RDTSUntil.Add(-RDTSWarning)) {
		w.alertOnce(ctx, now, "rdts-exposure", fmt.Sprintf(
			"%d lock(s) not yet permanent: they could be mined on the BLAKE2b chain after %s if their twins exist there (see OPERATING, Locks)",
			left, w.RDTSUntil.UTC().Format("2006-01-02")))
	}
	return nil
}

func (w *Worker) lock(ctx context.Context, now time.Time) error {
	deposits, err := w.Store.DepositsIn(ctx, SHA256, Confirmed)
	if err != nil || len(deposits) == 0 {
		return err
	}
	// One lock at a time: each spends the anchor of the one before
	if sent, err := w.Store.LocksIn(ctx, Sent); err != nil || len(sent) > 0 {
		return err
	}
	var anchor *Lock
	if a, err := w.Store.Anchor(ctx); err == nil {
		anchor = &a
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if now.After(w.RDTSUntil.Add(-RDTSMargin)) && (anchor == nil || !anchor.Permanent) {
		// Near the end of the data rules, only a permanent anchor keeps a
		// new lock off the BLAKE2b chain
		w.alertOnce(ctx, now, "rdts", fmt.Sprintf(
			"no locks: the BLAKE2b chain's data rules end %s and no permanent anchor is there to chain to; donations wait",
			w.RDTSUntil.UTC().Format("2006-01-02")))
		return nil
	}
	if last, _ := w.Store.Setting(ctx, "last_lock_wait"); last != "" {
		if at, err := time.Parse(time.RFC3339, last); err == nil && now.Sub(at) < time.Hour {
			return nil
		}
	}
	var coins []Coin
	var ids []int64
	for _, d := range deposits {
		if len(coins) >= w.MaxTxInputs {
			break // the rest next pass
		}
		spend, err := w.Sha256.Outspend(ctx, d.Txid, d.Vout)
		if err != nil {
			continue // this coin next pass; the others go on
		}
		if spend.Spent {
			// Ours after all (a lock taken for failed that went out)?
			if l, err := w.Store.LockByTxid(ctx, spend.Txid); err == nil {
				if err := w.Store.ReviveLock(ctx, l.ID, []int64{d.ID}); err != nil {
					return err
				}
				continue
			}
			w.Logger.Printf("ALERT: SHA256 donation %s was spent by %s, not by a lock of ours",
				d.Outpoint(), spend.Txid)
			d.State = Gone
			if err := w.Store.SaveDeposit(ctx, d); err != nil {
				return err
			}
			continue
		}
		r, err := w.Store.GetRequest(ctx, d.RequestID)
		if err != nil {
			return err
		}
		coins = append(coins, Coin{Txid: d.Txid, Vout: d.Vout, ValueSat: d.ValueSat,
			KeyIndex: uint32(r.KeyIndex), Branch: Requests})
		ids = append(ids, d.ID)
	}
	if len(coins) == 0 {
		return nil
	}
	rate, err := w.Sha256.FeeRate(ctx, 6)
	if err != nil || rate <= 0 {
		return fmt.Errorf("no SHA256-chain fee estimate: %v", err)
	}
	if rate > w.SweepMaxFee {
		w.alertOnce(ctx, now, "lock-fee", fmt.Sprintf(
			"not locking: the SHA256-chain fee rate is %.1f sat/vB, above %.0f", rate, w.SweepMaxFee))
		return nil
	}
	j, err := w.Store.NextLockIndex(ctx)
	if err != nil {
		return err
	}
	var anchorCoin *Coin
	if anchor != nil {
		spend, err := w.Sha256.Outspend(ctx, anchor.Txid, uint32(anchor.AnchorVout))
		if err != nil {
			return nil // next pass
		}
		if spend.Spent {
			w.alertOnce(ctx, now, "anchor-spent", fmt.Sprintf(
				"the anchor %s:%d was spent by %s, not by a lock of ours: not locking", anchor.Txid, anchor.AnchorVout, spend.Txid))
			return nil
		}
		anchorCoin = &Coin{Txid: anchor.Txid, Vout: uint32(anchor.AnchorVout), ValueSat: AnchorSat,
			KeyIndex: uint32(anchor.KeyIndex), Branch: Locks}
	}
	s, err := SignLock(w.Keys, coins, uint32(j), anchorCoin, rate, w.Params)
	if errors.Is(err, ErrTooSmall) {
		// More may come; an hour's wait spares the explorers meanwhile
		_ = w.Store.SetSetting(ctx, "last_lock_wait", now.UTC().Format(time.RFC3339))
		return nil
	}
	if err != nil {
		return err
	}
	// The BLAKE2b node must refuse it: for its data output when the coins
	// are there, or for a missing coin
	acc, err := w.Node.TestAccept(ctx, s.Raw)
	if err != nil {
		return fmt.Errorf("our node: %w", err)
	}
	if acc.Allowed {
		w.alertOnce(ctx, now, "lock-taken", "not locking: the BLAKE2b node would take the lock "+s.Txid)
		return nil
	}
	l := Lock{Txid: s.Txid, Raw: s.Raw, KeyIndex: j, ValueSat: s.AmountSat, FeeSat: s.FeeSat,
		State: Sent, AnchorVout: 1}
	if anchor != nil {
		l.AnchorIn = anchor.ID
	}
	id, err := w.Store.CreateLock(ctx, l, ids)
	if err != nil {
		return err
	}
	l.ID = id
	if _, err := w.Sha256.Broadcast(ctx, s.Raw); err != nil {
		// An explorer may have relayed it and answered badly: if one has
		// it, it went out, and trackLocks follows it
		if _, terr := w.Sha256.Tx(ctx, s.Txid); terr == nil {
			return nil
		}
		l.State, l.Error = FailedSend, err.Error()
		_ = w.Store.SetSetting(ctx, "last_lock_wait", now.UTC().Format(time.RFC3339))
		w.alertOnce(ctx, now, "lock-relay", fmt.Sprintf("lock %s not relayed (tried again in an hour): %v", s.Txid, err))
		return w.Store.EndLock(ctx, l)
	}
	w.Logger.Printf("locked %d SHA256-chain donation(s) in %s (BLAKE2b node: %s)", len(coins), s.Txid, acc.Reason)
	return nil
}

// trackLocks follows locks until they are deep enough, relaying again any
// that the SHA256 chain lost.
func (w *Worker) trackLocks(ctx context.Context) error {
	sent, err := w.Store.LocksIn(ctx, Sent)
	if err != nil || len(sent) == 0 {
		return err
	}
	tip, err := w.Sha256.TipHeight(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range sent {
		tx, err := w.Sha256.Tx(ctx, l.Txid)
		switch {
		case errors.Is(err, esplora.ErrNotFound):
			if _, err := w.Sha256.Broadcast(ctx, l.Raw); err != nil {
				l.Error = err.Error()
				errs = append(errs, w.Store.SaveLock(ctx, l))
			}
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if !tx.Status.Confirmed {
			continue
		}
		l.Confirmations = tip - tx.Status.BlockHeight + 1
		if l.Confirmations >= w.LockConfs {
			l.State = Done
			errs = append(errs, w.Store.EndLock(ctx, l))
			continue
		}
		errs = append(errs, w.Store.SaveLock(ctx, l))
	}
	return errors.Join(errs...)
}

// sweep sends the locks' outputs on to the operator's address, at most
// every SweepEvery, when they come to SweepMinSat. A lock can never exist
// on the BLAKE2b chain, so neither can a sweep of its output: our BLAKE2b
// node must refuse it for a missing coin before it is sent.
func (w *Worker) sweep(ctx context.Context, now time.Time) error {
	if w.SweepAddress == "" {
		return nil
	}
	if last, _ := w.Store.Setting(ctx, SettingLastSweep); last != "" {
		if at, err := time.Parse(time.RFC3339, last); err == nil && now.Sub(at) < w.SweepEvery {
			return nil
		}
	}
	locks, err := w.Store.Unswept(ctx)
	if err != nil || len(locks) == 0 {
		return err
	}
	_ = w.Store.SetSetting(ctx, SettingLastSweep, now.UTC().Format(time.RFC3339))
	var coins []Coin
	var ids []int64
	var total int64
	for _, l := range locks {
		if len(coins) >= w.MaxTxInputs {
			break
		}
		spend, err := w.Sha256.Outspend(ctx, l.Txid, 0)
		if err != nil {
			continue
		}
		if spend.Spent {
			if sw, err := w.Store.SweepByTxid(ctx, spend.Txid); err == nil {
				if err := w.Store.ReviveSweep(ctx, sw.ID, []int64{l.ID}); err != nil {
					return err
				}
				continue
			}
			w.Logger.Printf("ALERT: lock output %s:0 was spent by %s, not by a sweep of ours", l.Txid, spend.Txid)
			continue
		}
		coins = append(coins, Coin{Txid: l.Txid, Vout: 0, ValueSat: l.ValueSat,
			KeyIndex: uint32(l.KeyIndex), Branch: Locks})
		ids = append(ids, l.ID)
		total += l.ValueSat
	}
	if total < w.SweepMinSat {
		return nil
	}
	rate, err := w.Sha256.FeeRate(ctx, 6)
	if err != nil || rate <= 0 {
		return fmt.Errorf("no SHA256-chain fee estimate: %v", err)
	}
	if rate > w.SweepMaxFee {
		w.Logger.Printf("not sweeping: the SHA256-chain fee rate is %.1f sat/vB, above %.0f", rate, w.SweepMaxFee)
		return nil
	}
	s, err := SignSHA256(w.Keys, coins, w.SweepAddress, rate, w.Params)
	if errors.Is(err, ErrTooSmall) {
		return nil
	}
	if err != nil {
		return err
	}
	acc, err := w.Node.TestAccept(ctx, s.Raw)
	if err != nil {
		return fmt.Errorf("our node: %w", err)
	}
	if acc.Allowed || !(strings.Contains(acc.Reason, "missingorspent") ||
		strings.Contains(acc.Reason, "missing-inputs")) {
		w.Logger.Printf("ALERT: not sweeping: the BLAKE2b node does not refuse the sweep for a missing coin (allowed %v, %q)",
			acc.Allowed, acc.Reason)
		return nil
	}
	sw := Sweep{Txid: s.Txid, Raw: s.Raw, Dest: w.SweepAddress, AmountSat: s.AmountSat,
		FeeSat: s.FeeSat, State: Sent}
	id, err := w.Store.CreateSweep(ctx, sw, ids)
	if err != nil {
		return err
	}
	sw.ID = id
	if _, err := w.Sha256.Broadcast(ctx, s.Raw); err != nil {
		if _, terr := w.Sha256.Tx(ctx, s.Txid); terr == nil {
			return nil
		}
		sw.State, sw.Error = FailedSend, err.Error()
		w.Logger.Printf("ALERT: sweep %s not relayed: %v", s.Txid, err)
		return w.Store.EndSweep(ctx, sw)
	}
	w.Logger.Printf("swept %d sat of SHA256-chain donations to %s in %s (fee %d)",
		s.AmountSat, w.SweepAddress, s.Txid, s.FeeSat)
	return nil
}

// trackSweeps follows sweeps until they are deep enough, relaying again
// any that the SHA256 chain lost.
func (w *Worker) trackSweeps(ctx context.Context) error {
	sent, err := w.Store.SweepsIn(ctx, Sent)
	if err != nil || len(sent) == 0 {
		return err
	}
	tip, err := w.Sha256.TipHeight(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, sw := range sent {
		tx, err := w.Sha256.Tx(ctx, sw.Txid)
		switch {
		case errors.Is(err, esplora.ErrNotFound):
			if _, err := w.Sha256.Broadcast(ctx, sw.Raw); err != nil {
				sw.Error = err.Error()
				w.Logger.Printf("ALERT: sweep %s cannot be relayed again: %v", sw.Txid, err)
				errs = append(errs, w.Store.SaveSweep(ctx, sw))
			}
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if !tx.Status.Confirmed {
			continue
		}
		sw.Confirmations = tip - tx.Status.BlockHeight + 1
		if sw.Confirmations >= w.Sha256Confs {
			sw.State = Done
			errs = append(errs, w.Store.EndSweep(ctx, sw))
			continue
		}
		errs = append(errs, w.Store.SaveSweep(ctx, sw))
	}
	return errors.Join(errs...)
}

// sha256Consensus: the SHA256 chain's script rules (consensus, not policy)
const sha256Consensus = txscript.ScriptBip16 | txscript.ScriptVerifyDERSignatures |
	txscript.ScriptVerifyCheckLockTimeVerify | txscript.ScriptVerifyCheckSequenceVerify |
	txscript.ScriptVerifyWitness | txscript.ScriptStrictMultiSig | txscript.ScriptVerifyTaproot

// verifyInputs runs each input of a SHA256-chain transaction against the
// spent outputs an explorer gave: a wrong script or amount fails.
func verifyInputs(raw string, tx *esplora.Tx) error {
	b, err := hex.DecodeString(raw)
	if err != nil {
		return err
	}
	msg := wire.NewMsgTx(2)
	if err := msg.Deserialize(bytes.NewReader(b)); err != nil {
		return err
	}
	if len(msg.TxIn) != len(tx.Vin) {
		return errors.New("inputs do not match")
	}
	f := txscript.NewMultiPrevOutFetcher(nil)
	for i, vin := range tx.Vin {
		op := msg.TxIn[i].PreviousOutPoint
		if vin.Prevout == nil || op.Hash.String() != vin.Txid || op.Index != vin.Vout {
			return errors.New("a spent output is missing")
		}
		script, err := hex.DecodeString(vin.Prevout.ScriptPubKey)
		if err != nil {
			return errors.New("a spent output is missing")
		}
		f.AddPrevOut(op, wire.NewTxOut(vin.Prevout.Value, script))
	}
	sh := txscript.NewTxSigHashes(msg, f)
	for i := range msg.TxIn {
		prev := f.FetchPrevOutput(msg.TxIn[i].PreviousOutPoint)
		vm, err := txscript.NewEngine(prev.PkScript, msg, i, sha256Consensus, nil, sh, prev.Value, f)
		if err != nil {
			return err
		}
		if err := vm.Execute(); err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
	}
	return nil
}
