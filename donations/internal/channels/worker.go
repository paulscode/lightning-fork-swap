package channels

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/guard"
	"github.com/paulscode/lightning-fork-swap/donations/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/donations/internal/netcheck"
)

// Lnd is what the worker asks of lnd (with a macaroon the guard watches).
type Lnd interface {
	GetInfo(ctx context.Context) (lnd.Info, error)
	NewAddress(ctx context.Context) (string, error)
	ListUnspent(ctx context.Context, minConfs int) ([]lnd.Utxo, error)
	ListLeases(ctx context.Context) ([]lnd.Lease, error)
	LeaseOutput(ctx context.Context, id []byte, op lnd.OutPoint, seconds uint64) error
	ReleaseOutput(ctx context.Context, id []byte, op lnd.OutPoint) error
	FeeRate(ctx context.Context, target int) (uint64, error)
	GetNodeInfo(ctx context.Context, pubkey string) (lnd.Node, error)
	ConnectPeer(ctx context.Context, pubkey, host string, timeout int) error
	ListPeers(ctx context.Context) ([]lnd.Peer, error)
	OpenChannel(ctx context.Context, r lnd.OpenRequest) (string, error)
	PendingOpens(ctx context.Context) ([]lnd.PendingChannel, error)
	ListChannels(ctx context.Context) ([]lnd.Channel, error)
	ClosedChannels(ctx context.Context) ([]lnd.ClosedChannel, error)
	Transactions(ctx context.Context, startHeight int64) ([]lnd.Transaction, error)
}

// Settings the worker publishes for the API (the donation window's
// numbers)
const (
	SettingFeeRate  = "fee_rate"
	SettingMinSat   = "min_sat"
	SettingOurNode  = "our_node"
	SettingLastPass = "last_pass"
	SettingPruned   = "pruned"
	// Addresses ever handed out (never lowered by pruning)
	SettingAddressesIssued = "addresses_issued"
)

// Worker runs the orders.
type Worker struct {
	Store  Store
	Lnd    Lnd
	Rules  Rules
	Now    func() time.Time
	Logger *log.Logger

	// per pass
	info     lnd.Info
	rate     uint64
	unspent  map[string][]lnd.Utxo // by address
	leased   map[string]bool       // our leases, by outpoint
	opened   bool                  // one open per pass
	channels []lnd.Channel
	pending  []lnd.PendingChannel
	closed   []lnd.ClosedChannel
	active   []Order
}

// FundingVsize estimates the funding transaction: n taproot inputs, the
// channel output and a change output.
func FundingVsize(inputs int) int64 { return 11 + 58*int64(inputs) + 43 + 43 }

// MinDonation is the least a channel donation must bring at a fee rate:
// the minimum channel and the fee of opening it from one coin.
func (r Rules) MinDonation(rate uint64) int64 {
	return r.MinChannelSat + int64(rate)*FundingVsize(1)
}

// Pass runs every order once. An error means lnd or the database could
// not be read; the orders wait for the next pass.
func (w *Worker) Pass(ctx context.Context) error {
	now := w.Now()
	info, err := w.Lnd.GetInfo(ctx)
	if err != nil {
		return fmt.Errorf("lnd: %w", err)
	}
	w.info = info
	w.rate = w.Rules.FeeFloor
	if rate, err := w.Lnd.FeeRate(ctx, 6); err == nil && rate > w.rate {
		w.rate = rate
	}
	if w.rate > w.Rules.FeeCeiling {
		w.rate = w.Rules.FeeCeiling
	}
	_ = w.Store.SetSetting(ctx, SettingFeeRate, strconv.FormatUint(w.rate, 10))
	_ = w.Store.SetSetting(ctx, SettingMinSat, strconv.FormatInt(w.Rules.MinDonation(w.rate), 10))
	_ = w.Store.SetSetting(ctx, SettingOurNode, info.IdentityPubkey)

	if err := w.applyRequests(ctx); err != nil {
		return err
	}
	if w.unspent, w.leased, err = w.coins(ctx); err != nil {
		return fmt.Errorf("lnd: %w", err)
	}
	w.channels, w.pending, w.closed = nil, nil, nil
	w.opened = false
	if w.active, err = w.Store.Active(ctx); err != nil {
		return err
	}
	for i := range w.active {
		o := w.active[i]
		events, err := w.step(ctx, &o, now)
		if err != nil {
			if w.Logger != nil {
				w.Logger.Printf("order %s: %v", o.ID, err)
			}
			continue
		}
		if events != nil {
			if err := w.Store.Save(ctx, o, events...); err != nil {
				return err
			}
			w.active[i] = o
		}
	}
	if last, _ := w.Store.Setting(ctx, SettingPruned); last == "" ||
		last < now.Add(-24*time.Hour).UTC().Format(time.RFC3339) {
		if err := w.Store.Prune(ctx, now.Add(-w.Rules.PruneAfter)); err == nil {
			_ = w.Store.SetSetting(ctx, SettingPruned, now.UTC().Format(time.RFC3339))
		}
	}
	return w.Store.SetSetting(ctx, SettingLastPass, now.UTC().Format(time.RFC3339))
}

// coins indexes the wallet's unspent outputs by address, and our leases.
// Leased outputs are not in lnd's unspent list.
func (w *Worker) coins(ctx context.Context) (map[string][]lnd.Utxo, map[string]bool, error) {
	utxos, err := w.Lnd.ListUnspent(ctx, 0)
	if err != nil {
		return nil, nil, err
	}
	byAddress := map[string][]lnd.Utxo{}
	for _, u := range utxos {
		byAddress[u.Address] = append(byAddress[u.Address], u)
	}
	leases, err := w.Lnd.ListLeases(ctx)
	if err != nil {
		return nil, nil, err
	}
	ours := map[string]bool{}
	id := leaseID()
	for _, l := range leases {
		if l.ID == id {
			ours[strings.ToLower(l.Outpoint.String())] = true
		}
	}
	return byAddress, ours, nil
}

func leaseID() string {
	return base64.StdEncoding.EncodeToString(guard.LeaseID)
}

func event(now time.Time, kind string, detail map[string]any) Event {
	return Event{At: now, Kind: kind, Detail: detail}
}

// step moves one order on; nil events means nothing changed.
func (w *Worker) step(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	switch o.State {
	case New:
		return w.issue(ctx, o, now)
	case AwaitingPayment, PaymentSeen:
		events, err := w.payment(ctx, o, now)
		if err != nil || o.State != PaymentConfirmed {
			return events, err
		}
		// Paid: on to the channel in the same pass
		return w.tryOpen(ctx, o, now, events)
	case PaymentConfirmed, Connecting:
		return w.tryOpen(ctx, o, now, nil)
	case Retrying:
		if o.FailingSince != nil && now.Sub(*o.FailingSince) > w.Rules.RetryWindow {
			return w.fallBack(ctx, o, now, ErrGaveUp), nil
		}
		if o.NextAttempt == nil || !now.Before(*o.NextAttempt) {
			return w.tryOpen(ctx, o, now, nil)
		}
	case NeedsAttention:
		if o.AttentionSince != nil && now.Sub(*o.AttentionSince) > w.Rules.AttentionWindow {
			return w.fallBack(ctx, o, now, ErrNoEdit), nil
		}
	case Opening:
		return w.resume(ctx, o, now)
	case FundingBroadcast:
		return w.funding(ctx, o, now)
	case Open:
		return w.watchOpen(ctx, o, now)
	}
	return nil, nil
}

func (w *Worker) issue(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	unpaid := 0
	for _, other := range w.active {
		if other.State == AwaitingPayment && other.ReceivedSat == 0 {
			unpaid++
		}
	}
	if unpaid >= w.Rules.MaxUnpaid {
		o.State, o.ErrorCode = Rejected, ErrBudget
		return []Event{event(now, "rejected", map[string]any{"code": ErrBudget})}, nil
	}
	address, err := w.Lnd.NewAddress(ctx)
	if err != nil {
		return nil, err
	}
	// Counted for a seed restore: lnd must look past this many unused
	// addresses (restore.sh widens its recovery window by it)
	issued, _ := w.Store.Setting(ctx, SettingAddressesIssued)
	n, _ := strconv.ParseInt(issued, 10, 64)
	if err := w.Store.SetSetting(ctx, SettingAddressesIssued, strconv.FormatInt(n+1, 10)); err != nil {
		return nil, err
	}
	o.Address, o.State = address, AwaitingPayment
	// The time to pay runs from the address
	if max := now.Add(w.Rules.Expiry); o.ExpiresAt.IsZero() || o.ExpiresAt.After(max) ||
		o.ExpiresAt.Before(now) {
		o.ExpiresAt = max
	}
	return []Event{event(now, "address", map[string]any{"address": address})}, nil
}

// payment follows what reaches the order's address until enough is
// confirmed, then leases exactly those coins for the channel.
func (w *Worker) payment(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	coins := append([]lnd.Utxo(nil), w.unspent[o.Address]...)
	// The oldest first; at most MaxUtxos count (more is general liquidity)
	sort.Slice(coins, func(i, j int) bool {
		if coins[i].Confirmations != coins[j].Confirmations {
			return coins[i].Confirmations > coins[j].Confirmations
		}
		return coins[i].Outpoint.String() < coins[j].Outpoint.String()
	})
	if len(coins) > w.Rules.MaxUtxos {
		coins = coins[:w.Rules.MaxUtxos]
	}
	var received, confirmed int64
	var utxos []Utxo
	for _, c := range coins {
		received += int64(c.AmountSat)
		if int64(c.Confirmations) >= w.Rules.Confirmations {
			confirmed += int64(c.AmountSat)
		}
		utxos = append(utxos, Utxo{Outpoint: strings.ToLower(c.Outpoint.String()),
			AmountSat: int64(c.AmountSat), Confirmations: int64(c.Confirmations)})
	}
	var events []Event
	if received < o.ReceivedSat && len(o.Utxos) > len(utxos) {
		events = append(events, event(now, "payment_gone", nil))
	}
	changed := received != o.ReceivedSat || confirmed != o.ConfirmedSat ||
		!sameUtxos(utxos, o.Utxos)
	if received > o.ReceivedSat {
		events = append(events, event(now, "payment_seen", map[string]any{"receivedSat": received}))
	}
	o.ReceivedSat, o.ConfirmedSat, o.Utxos = received, confirmed, utxos
	if received > 0 {
		o.State = PaymentSeen
	} else {
		o.State = AwaitingPayment
	}

	min := w.Rules.MinDonation(w.rate)
	if confirmed >= min {
		// Lease the confirmed coins: from now on only this channel can
		// spend them
		var leased []Utxo
		var total int64
		for _, u := range utxos {
			if u.Confirmations < w.Rules.Confirmations {
				continue
			}
			if err := w.Lnd.LeaseOutput(ctx, guard.LeaseID, outpoint(u.Outpoint),
				w.Rules.LeaseSeconds); err != nil {
				// Leased coins leave lnd's unspent list: undo the ones
				// leased so far, so the next pass sees the payment whole
				for _, l := range leased {
					_ = w.Lnd.ReleaseOutput(ctx, guard.LeaseID, outpoint(l.Outpoint))
				}
				return nil, err
			}
			u.Leased = true
			leased = append(leased, u)
			total += u.AmountSat
		}
		o.Utxos, o.ConfirmedSat, o.State = leased, total, PaymentConfirmed
		events = append(events, event(now, "payment_confirmed", map[string]any{"confirmedSat": total}))
		return events, nil
	}
	if now.After(o.ExpiresAt) {
		switch {
		case received == 0:
			o.State = Expired
			return append(events, event(now, "expired", nil)), nil
		case received < min || now.After(o.ExpiresAt.Add(w.Rules.Expiry)):
			// Too little, or enough that never confirmed in another day
			return append(events, w.fallBack(ctx, o, now, ErrTooLittle)...), nil
		}
	}
	if !changed && len(events) == 0 {
		return nil, nil
	}
	if events == nil {
		events = []Event{event(now, "payment_progress", map[string]any{
			"receivedSat": received, "confirmedSat": confirmed})}
	}
	return events, nil
}

// justLeased: the coins were leased in this pass (the pass's lease list
// was read before)
func justLeased(prior []Event) bool {
	for _, e := range prior {
		if e.Kind == "payment_confirmed" {
			return true
		}
	}
	return false
}

func sameUtxos(a, b []Utxo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func outpoint(s string) lnd.OutPoint {
	i := strings.LastIndexByte(s, ':')
	index, _ := strconv.ParseUint(s[i+1:], 10, 32)
	return lnd.OutPoint{TxidStr: s[:i], OutputIndex: uint32(index)}
}

// tryOpen connects to the donor's node and opens the channel from the
// order's leased coins. prior are the pass's events so far for the order.
func (w *Worker) tryOpen(ctx context.Context, o *Order, now time.Time, prior []Event) ([]Event, error) {
	if w.opened {
		return prior, nil
	}
	events := prior
	if o.NodePubkey == w.info.IdentityPubkey {
		return append(events, w.attention(o, now, ErrOurNode)...), nil
	}
	held := 0
	for _, other := range w.active {
		if other.ID != o.ID && other.NodePubkey == o.NodePubkey &&
			(other.State == Open || other.State == FundingBroadcast || other.State == Opening) {
			held++
		}
	}
	if held >= w.Rules.MaxPerNode {
		return append(events, w.attention(o, now, ErrNodeLimit)...), nil
	}
	// All the order's coins must still be ours: leased, or (after an
	// open that failed) unspent and leased again now
	for i, u := range o.Utxos {
		if w.leased[u.Outpoint] || justLeased(prior) {
			continue
		}
		if !w.isUnspent(o.Address, u.Outpoint) {
			more, err := w.lostCoins(ctx, o, now)
			return append(events, more...), err
		}
		if err := w.Lnd.LeaseOutput(ctx, guard.LeaseID, outpoint(u.Outpoint),
			w.Rules.LeaseSeconds); err != nil {
			return events, err
		}
		w.leased[u.Outpoint] = true
		o.Utxos[i].Leased = true
	}

	if node, err := w.Lnd.GetNodeInfo(ctx, o.NodePubkey); err == nil {
		if o.NodeAlias != node.Alias {
			o.NodeAlias = node.Alias
		}
		if o.NodeAddr == "" {
			for _, a := range node.Addresses {
				if addr, err := netcheck.Literal(a.Addr); err == nil {
					o.NodeAddr = addr
					break
				}
			}
		}
	}
	peers, err := w.Lnd.ListPeers(ctx)
	if err != nil {
		return append(events, w.failed(o, now, ErrLndUnavailable)...), nil
	}
	peer := findPeer(peers, o.NodePubkey)
	// Already connected (it may have a channel with us): nothing to dial
	if peer == nil {
		if o.NodeAddr == "" {
			return append(events, w.attention(o, now, ErrNoAddress)...), nil
		}
		addr, err := netcheck.Literal(o.NodeAddr)
		if err != nil {
			return append(events, w.attention(o, now, ErrNoAddress)...), nil
		}
		if o.State != Connecting {
			o.State = Connecting
			events = append(events, event(now, "connecting", map[string]any{"node": addr}))
		}
		w.opened = true
		if err := w.Lnd.ConnectPeer(ctx, o.NodePubkey, addr, 30); err != nil {
			return append(events, w.lndFailed(o, now, err)...), nil
		}
		if peers, err = w.Lnd.ListPeers(ctx); err != nil {
			return append(events, w.failed(o, now, ErrLndUnavailable)...), nil
		}
		peer = findPeer(peers, o.NodePubkey)
	} else if o.State != Connecting {
		o.State = Connecting
		events = append(events, event(now, "connecting", nil))
	}
	w.opened = true
	if peer == nil {
		return append(events, w.failed(o, now, ErrDisconnected)...), nil
	}
	// Bit 512/513: a node of the Lightning Fork network
	if !lnd.HasFeature(peer.Features, 512) {
		return append(events, w.attention(o, now, ErrNotForkNode)...), nil
	}
	o.Wumbo = lnd.HasFeature(peer.Features, 19)

	max := w.Rules.MaxChannelSat
	if o.Wumbo && w.Rules.MaxWumboSat > max {
		max = w.Rules.MaxWumboSat
	}
	var total int64
	var outpoints []lnd.OutPoint
	for _, u := range o.Utxos {
		total += u.AmountSat
		outpoints = append(outpoints, outpoint(u.Outpoint))
	}
	fee := int64(w.rate) * FundingVsize(len(outpoints))
	if total-fee < w.Rules.MinChannelSat {
		// Fees rose since the coins were confirmed: wait for them to fall
		return append(events, w.failed(o, now, "fees_high")...), nil
	}
	req := lnd.OpenRequest{NodePubkey: o.NodePubkey, SatPerVbyte: w.rate,
		Outpoints: outpoints, Memo: "donation:" + o.ID}
	if total-fee <= max {
		req.FundMax = true
		o.CapacitySat, o.RemainderSat = total-fee, 0
	} else {
		req.LocalFundingAmount = max
		o.CapacitySat, o.RemainderSat = max, total-max-fee
	}
	o.FeeSat = fee
	o.State = Opening
	events = append(events, event(now, "opening", map[string]any{"capacitySat": o.CapacitySat}))
	// Recorded before the call, with the pass's events so far: a crash
	// during it is found by resume()
	if err := w.Store.Save(ctx, *o, events...); err != nil {
		return nil, err
	}
	o.Version++
	// lnd opens only from coins that are not leased: release them just
	// before (the guard allows an open from coins we released a moment
	// ago), and lease them again if the open fails
	for _, op := range outpoints {
		if err := w.Lnd.ReleaseOutput(ctx, guard.LeaseID, op); err != nil {
			w.release(ctx, o)
			return w.lndFailed(o, now, err), nil
		}
	}
	point, err := w.Lnd.OpenChannel(ctx, req)
	if err != nil {
		// lnd may have opened it all the same (the call timed out while it
		// went on): a new pending channel to the node is this one
		w.channels, w.pending = nil, nil
		if adopted, _ := w.adopt(ctx, o, now); adopted != nil {
			return adopted, nil
		}
		w.release(ctx, o)
		return w.lndFailed(o, now, err), nil
	}
	o.ChannelPoint, o.State = point, FundingBroadcast
	o.ErrorCode, o.NextAttempt = "", nil
	return []Event{event(now, "funding_broadcast", map[string]any{
		"channelPoint": point, "capacitySat": o.CapacitySat, "remainderSat": o.RemainderSat})}, nil
}

// release leases an order's coins again after an open did not use them
// (a coin it cannot lease is found unleased next pass and leased then).
func (w *Worker) release(ctx context.Context, o *Order) {
	for _, u := range o.Utxos {
		_ = w.Lnd.LeaseOutput(ctx, guard.LeaseID, outpoint(u.Outpoint), w.Rules.LeaseSeconds)
	}
}

func (w *Worker) isUnspent(address, op string) bool {
	for _, u := range w.unspent[address] {
		if strings.EqualFold(u.Outpoint.String(), op) {
			return true
		}
	}
	return false
}

func findPeer(peers []lnd.Peer, pubkey string) *lnd.Peer {
	for i := range peers {
		if peers[i].PubKey == pubkey {
			return &peers[i]
		}
	}
	return nil
}

// resume finds what happened to an open that was interrupted (the worker
// stopped during the call).
func (w *Worker) resume(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	allLeased := true
	for _, u := range o.Utxos {
		if !w.leased[u.Outpoint] && !w.isUnspent(o.Address, u.Outpoint) {
			allLeased = false
		}
	}
	if allLeased {
		// Nothing was spent (the coins are leased, or released just
		// before the open and still unspent): try again
		o.State = Connecting
		return []Event{event(now, "resuming", nil)}, nil
	}
	adopted, err := w.adopt(ctx, o, now)
	if err != nil || adopted != nil {
		return adopted, err
	}
	return w.lostCoins(ctx, o, now)
}

// adopt takes a channel to the order's node that no order has: the one
// this order's open made when its answer was lost. Its memo says whose it
// is when lnd tells; otherwise the node does.
func (w *Worker) adopt(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	if err := w.loadChannels(ctx); err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, other := range w.active {
		if other.ChannelPoint != "" {
			taken[other.ChannelPoint] = true
		}
	}
	take := func(point string) []Event {
		o.ChannelPoint, o.State, o.ErrorCode, o.NextAttempt = point, FundingBroadcast, "", nil
		return []Event{event(now, "funding_broadcast", map[string]any{
			"channelPoint": point, "capacitySat": o.CapacitySat})}
	}
	for _, p := range w.pending {
		if p.RemoteNodePub == o.NodePubkey && !taken[p.ChannelPoint] &&
			(p.Memo == "" || p.Memo == "donation:"+o.ID) {
			return take(p.ChannelPoint), nil
		}
	}
	for _, c := range w.channels {
		if c.RemotePubkey == o.NodePubkey && c.Initiator && !taken[c.ChannelPoint] &&
			(c.Memo == "" || c.Memo == "donation:"+o.ID) {
			return take(c.ChannelPoint), nil
		}
	}
	return nil, nil
}

// lostCoins: the order's coins are no longer leased to us and no channel
// took them. Should not happen (the guard lets nothing else spend them);
// kept as a general donation, for the operator to look at.
func (w *Worker) lostCoins(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	if w.Logger != nil {
		w.Logger.Printf("order %s: its coins are no longer leased", o.ID)
	}
	return w.fallBack(ctx, o, now, ErrInternal), nil
}

func (w *Worker) loadChannels(ctx context.Context) error {
	if w.channels != nil || w.pending != nil {
		return nil
	}
	var err error
	if w.pending, err = w.Lnd.PendingOpens(ctx); err != nil {
		return err
	}
	if w.channels, err = w.Lnd.ListChannels(ctx); err != nil {
		return err
	}
	if w.pending == nil {
		w.pending = []lnd.PendingChannel{}
	}
	return nil
}

// funding waits for the channel's transaction to confirm.
func (w *Worker) funding(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	if err := w.loadChannels(ctx); err != nil {
		return nil, err
	}
	for _, c := range w.channels {
		if c.ChannelPoint == o.ChannelPoint {
			o.State, o.OpenedAt, o.CapacitySat = Open, &now, int64(c.Capacity)
			return []Event{event(now, "open", map[string]any{"capacitySat": int64(c.Capacity)})}, nil
		}
	}
	// Still pending: how many confirmations
	txid := o.ChannelPoint[:strings.IndexByte(o.ChannelPoint, ':')]
	txs, err := w.Lnd.Transactions(ctx, max64(0, w.info.BlockHeight-2016))
	if err != nil {
		return nil, err
	}
	for _, t := range txs {
		if t.TxHash == txid && t.NumConfirmations != o.FundingConfs {
			event := event(now, "funding_confirmations", map[string]any{"confirmations": t.NumConfirmations})
			if t.NumConfirmations < o.FundingConfs {
				event.Kind = "funding_reorged"
			}
			o.FundingConfs = t.NumConfirmations
			return []Event{event}, nil
		}
	}
	return nil, nil
}

func (w *Worker) watchOpen(ctx context.Context, o *Order, now time.Time) ([]Event, error) {
	if w.closed == nil {
		closed, err := w.Lnd.ClosedChannels(ctx)
		if err != nil {
			return nil, err
		}
		w.closed = append([]lnd.ClosedChannel{}, closed...)
	}
	for _, c := range w.closed {
		if c.ChannelPoint == o.ChannelPoint {
			o.State, o.ClosedAt = Closed, &now
			return []Event{event(now, "closed", map[string]any{"closeType": c.CloseType})}, nil
		}
	}
	return nil, nil
}

// lndFailed: lnd's own words go to the operator's log; the donor sees a
// code.
func (w *Worker) lndFailed(o *Order, now time.Time, err error) []Event {
	if w.Logger != nil {
		w.Logger.Printf("order %s: %v", o.ID, err)
	}
	return w.failed(o, now, classify(err))
}

// failed schedules another try, or asks the donor to act.
func (w *Worker) failed(o *Order, now time.Time, code string) []Event {
	switch code {
	case ErrWrongNode, ErrNotForkNode, ErrNoAddress:
		return w.attention(o, now, code)
	case ErrRejected, ErrRejectedSize:
		// Their node's rules: one more try, then the donor's turn
		if o.Attempts > 0 && o.ErrorCode == code {
			return w.attention(o, now, code)
		}
	}
	o.Attempts++
	if o.FailingSince == nil {
		o.FailingSince = &now
	}
	next := now.Add(RetryAfter(o.Attempts))
	o.NextAttempt, o.ErrorCode, o.State = &next, code, Retrying
	return []Event{event(now, "retry", map[string]any{"code": code, "next": next})}
}

func (w *Worker) attention(o *Order, now time.Time, code string) []Event {
	if o.State == NeedsAttention && o.ErrorCode == code {
		return nil
	}
	o.State, o.ErrorCode, o.NextAttempt = NeedsAttention, code, nil
	if o.AttentionSince == nil {
		o.AttentionSince = &now
	}
	return []Event{event(now, "needs_attention", map[string]any{"code": code})}
}

// fallBack ends an order as a general donation: its coins are released
// to lnd's wallet (they are the service's either way).
func (w *Worker) fallBack(ctx context.Context, o *Order, now time.Time, code string) []Event {
	for i, u := range o.Utxos {
		if u.Leased {
			if err := w.Lnd.ReleaseOutput(ctx, guard.LeaseID, outpoint(u.Outpoint)); err == nil {
				o.Utxos[i].Leased = false
			}
		}
	}
	o.State, o.ErrorCode, o.NextAttempt = FellBack, code, nil
	return []Event{event(now, "fell_back", map[string]any{"code": code})}
}

// applyRequests takes the donors' edits.
func (w *Worker) applyRequests(ctx context.Context) error {
	reqs, err := w.Store.Requests(ctx)
	if err != nil {
		return err
	}
	now := w.Now()
	for _, r := range reqs {
		o, err := w.Store.GetOrder(ctx, r.OrderID)
		// The operator's (donations orders retry|fallback)
		if err == nil && r.Kind == "retry" && (o.State == Retrying || o.State == NeedsAttention) {
			o.State, o.NextAttempt, o.AttentionSince, o.FailingSince = Connecting, nil, nil, nil
			o.Attempts, o.ErrorCode = 0, ""
			if err := w.Store.Save(ctx, o, event(now, "retry_now", nil)); err != nil {
				return err
			}
		}
		if err == nil && r.Kind == "fallback" && !o.State.Final() && o.State != Open &&
			o.State != FundingBroadcast && o.State != Opening {
			if err := w.Store.Save(ctx, o, w.fallBack(ctx, &o, now, "operator")...); err != nil {
				return err
			}
		}
		if err == nil && r.Kind == "node" && o.State.Editable() {
			o.NodePubkey, o.NodeAddr, o.NodeInput, o.NodeAlias = r.Node, r.Addr, r.Input, ""
			o.Attempts, o.ErrorCode, o.AttentionSince, o.FailingSince = 0, "", nil, nil
			o.NextAttempt = nil
			if o.State.Paid() {
				o.State = Connecting
			}
			if err := w.Store.Save(ctx, o, event(now, "node_changed", nil)); err != nil {
				return err
			}
		}
		if err := w.Store.DoneRequest(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// classify turns lnd's error into a code for the donor.
func classify(err error) string {
	var e *lnd.Error
	msg := strings.ToLower(err.Error())
	if errors.As(err, &e) {
		msg = strings.ToLower(e.Message)
	}
	switch {
	case containsAny(msg, "pubkey mismatch", "remote static key", "handshake", "brontide",
		"act one", "act two", "act three"):
		return ErrWrongNode
	case containsAny(msg, "below min chan size", "chan size of", "channel too small",
		"min_chan_size", "minimum channel size"):
		return ErrRejectedSize
	case containsAny(msg, "pending channels exceed", "too many pending"):
		return ErrPending
	case containsAny(msg, "timeout", "timed out", "deadline exceeded", "connection refused",
		"no route to host", "network is unreachable", "unable to connect", "dial", "eof",
		"connection reset"):
		return ErrUnreachable
	case containsAny(msg, "not online", "disconnected", "peer exiting", "is not connected",
		"peer unavailable"):
		return ErrDisconnected
	case containsAny(msg, "funding failed", "rejected", "remote canceled", "received funding error",
		"channel open canceled"):
		return ErrRejected
	case containsAny(msg, "refused by the donations guard"):
		return ErrInternal
	}
	return ErrInternal
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Run passes every interval, and sooner when woken (a new order or edit).
func (w *Worker) Run(ctx context.Context, interval time.Duration, wake <-chan struct{}) {
	for {
		if err := w.Pass(ctx); err != nil && w.Logger != nil {
			w.Logger.Printf("pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(interval):
		}
	}
}
