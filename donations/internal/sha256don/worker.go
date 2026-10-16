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
	"github.com/btcsuite/btcd/wire"

	"github.com/paulscode/lightning-fork-swap/donations/internal/bip322"
	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/knots"
)

// Explorer is what the worker reads from a chain's explorer.
type Explorer interface {
	AddressTxs(ctx context.Context, address string) ([]esplora.Tx, error)
	Tx(ctx context.Context, txid string) (*esplora.Tx, error)
	TxHex(ctx context.Context, txid string) (string, error)
	TxStatus(ctx context.Context, txid string) (*esplora.Status, error)
	TipHeight(ctx context.Context) (int64, error)
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
}

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

func (w *Worker) due(ctx context.Context, r Request, now time.Time) bool {
	if r.LastPolled == nil {
		return true
	}
	// A donor told us their transaction since the last look
	if hints, err := w.Store.Hints(ctx, r.ID, ""); err == nil {
		for _, h := range hints {
			if h.At.After(*r.LastPolled) {
				return true
			}
		}
	}
	every := PollEvery(now.Sub(r.CreatedAt))
	return every >= 0 && now.Sub(*r.LastPolled) >= every
}

// Pass does one round.
func (w *Worker) Pass(ctx context.Context) error {
	w.defaults()
	now := w.Now()
	if err := w.assign(ctx); err != nil {
		return fmt.Errorf("assigning addresses: %w", err)
	}
	var errs []error
	requests, err := w.Store.Assigned(ctx)
	if err != nil {
		return err
	}
	var shaTip int64
	polls := 0
	for _, r := range requests {
		if polls >= w.MaxPolls || ctx.Err() != nil {
			break
		}
		if !w.due(ctx, r, now) {
			continue
		}
		polls++
		if shaTip == 0 {
			if shaTip, err = w.Sha256.TipHeight(ctx); err != nil {
				errs = append(errs, fmt.Errorf("SHA256 tip: %w", err))
				break
			}
		}
		if err := w.poll(ctx, r, shaTip); err != nil {
			errs = append(errs, fmt.Errorf("request %s: %w", r.ID, err))
			continue
		}
		if err := w.Store.Polled(ctx, r.ID, now); err != nil {
			errs = append(errs, err)
		}
	}
	if err := w.forward(ctx); err != nil {
		errs = append(errs, fmt.Errorf("forwarding: %w", err))
	}
	if err := w.track(ctx); err != nil {
		errs = append(errs, fmt.Errorf("following forwards: %w", err))
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
			confs := int64(0)
			if tx.Status.Confirmed && tx.Status.BlockHeight > 0 {
				confs = shaTip - tx.Status.BlockHeight + 1
			}
			d, err := w.Store.PutDeposit(ctx, Deposit{RequestID: r.ID, Chain: SHA256,
				Txid: tx.Txid, Vout: uint32(i), ValueSat: out.Value,
				Confirmations: confs, State: Seen})
			if err != nil {
				return err
			}
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
		if d.Chain == BLAKE2b && (d.State == Seen || d.State == Ready) {
			add(d.Txid, d.Vout)
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
		found[k] = true
		d, err := w.Store.PutDeposit(ctx, Deposit{RequestID: r.ID, Chain: BLAKE2b,
			Txid: txid, Vout: vout, ValueSat: out.ValueSat,
			Confirmations: out.Confirmations, State: Seen})
		if err != nil {
			return err
		}
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
	// The explorer's bytes must be the transaction asked about
	if got, err := txidOf(raw); err != nil || got != d.Txid {
		return d.CopyState, d.CopyReason, nil
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
func (w *Worker) forward(ctx context.Context) error {
	ready, err := w.Store.DepositsIn(ctx, BLAKE2b, Ready)
	if err != nil || len(ready) == 0 {
		return err
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
		if dest == "" {
			continue
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
			// Never relayed: its coins go back to Ready
			f.State, f.Error = FailedSend, acc.Reason
			w.Logger.Printf("ALERT: forward %s refused by our node: %s", s.Txid, acc.Reason)
			errs = append(errs, w.Store.EndForward(ctx, f))
			continue
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
			// Neither in the UTXO set, the mempool nor a block: relay again
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
