package channels

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/guard"
	"github.com/paulscode/lightning-fork-swap/donations/internal/guard/lnrpc"
	"github.com/paulscode/lightning-fork-swap/donations/internal/lnd"
)

const (
	ourNode   = "03cd3175b98f56a4b7a27d169ff211afe4d49973448fce92e2f7f03f526663e666"
	donorNode = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

// wallet simulates lnd for the worker: coins, leases, peers, channels.
type wallet struct {
	height     int64
	addresses  int
	unspent    []lnd.Utxo
	leases     map[string]bool
	nodes      map[string]lnd.Node
	reachable  map[string]bool   // host -> answers
	features   map[string]string // pubkey -> "fork", "fork wumbo", "sha256"
	connectErr error
	openErr    error
	peers      map[string]bool
	pending    []lnd.PendingChannel
	channels   []lnd.Channel
	closed     []lnd.ClosedChannel
	opens      []lnd.OpenRequest
	fee        uint64
	down       bool
	leaseErr   error
	txs        []lnd.Transaction
	released   *guard.Released
	// The open goes through but its answer is lost (a timeout)
	lostAnswer bool
	t          *testing.T
}

func newWallet(t *testing.T) *wallet {
	return &wallet{height: 976000, leases: map[string]bool{}, nodes: map[string]lnd.Node{},
		reachable: map[string]bool{"91.190.100.60:9735": true},
		features:  map[string]string{donorNode: "fork"}, peers: map[string]bool{}, fee: 2,
		released: guard.NewReleased(), t: t}
}

func (f *wallet) GetInfo(context.Context) (lnd.Info, error) {
	if f.down {
		return lnd.Info{}, errors.New("lnd down")
	}
	return lnd.Info{IdentityPubkey: ourNode, BlockHeight: f.height, SyncedToChain: true}, nil
}

func (f *wallet) NewAddress(context.Context) (string, error) {
	f.addresses++
	return fmt.Sprintf("bc1pdonation%d", f.addresses), nil
}

func (f *wallet) ListUnspent(context.Context, int) ([]lnd.Utxo, error) {
	var out []lnd.Utxo
	for _, u := range f.unspent {
		if !f.leases[u.Outpoint.String()] {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *wallet) ListLeases(context.Context) ([]lnd.Lease, error) {
	var out []lnd.Lease
	for op, ok := range f.leases {
		if ok {
			out = append(out, lnd.Lease{ID: base64.StdEncoding.EncodeToString(guard.LeaseID),
				Outpoint: outpoint(op)})
		}
	}
	// lnd's own leases (another id) are not ours
	out = append(out, lnd.Lease{ID: "b3RoZXI=", Outpoint: lnd.OutPoint{TxidStr: strings.Repeat("9", 64)}})
	return out, nil
}

func (f *wallet) LeaseOutput(_ context.Context, id []byte, op lnd.OutPoint, seconds uint64) error {
	if f.leaseErr != nil {
		return f.leaseErr
	}
	if string(id) != string(guard.LeaseID) || seconds == 0 {
		f.t.Fatalf("lease %x %d", id, seconds)
	}
	f.leases[op.String()] = true
	return nil
}

func (f *wallet) ReleaseOutput(_ context.Context, _ []byte, op lnd.OutPoint) error {
	delete(f.leases, op.String())
	f.released.Note(op.String())
	return nil
}

func (f *wallet) FeeRate(context.Context, int) (uint64, error) { return f.fee, nil }

func (f *wallet) GetNodeInfo(_ context.Context, pubkey string) (lnd.Node, error) {
	n, ok := f.nodes[pubkey]
	if !ok {
		return n, &lnd.Error{Status: 404, Message: "unable to find node"}
	}
	return n, nil
}

func (f *wallet) ConnectPeer(_ context.Context, pubkey, host string, timeout int) error {
	if f.connectErr != nil {
		return f.connectErr
	}
	if !f.reachable[host] {
		return &lnd.Error{Status: 500, Message: "dial tcp " + host + ": i/o timeout"}
	}
	f.peers[pubkey] = true
	return nil
}

func (f *wallet) ListPeers(context.Context) ([]lnd.Peer, error) {
	var out []lnd.Peer
	for p := range f.peers {
		features := map[string]lnd.Feature{}
		kind := f.features[p]
		if strings.Contains(kind, "fork") {
			features["513"] = lnd.Feature{Name: "blake2b"}
		}
		if strings.Contains(kind, "wumbo") {
			features["19"] = lnd.Feature{Name: "large-channels"}
		}
		out = append(out, lnd.Peer{PubKey: p, Features: features})
	}
	return out, nil
}

// OpenChannel checks the request as the guard would, spends the coins and
// publishes a funding transaction.
func (f *wallet) OpenChannel(ctx context.Context, r lnd.OpenRequest) (string, error) {
	if f.openErr != nil {
		return "", f.openErr
	}
	key, _ := hex.DecodeString(r.NodePubkey)
	req := &lnrpc.OpenChannelRequest{NodePubkey: key, LocalFundingAmount: r.LocalFundingAmount,
		FundMax: r.FundMax, SatPerVbyte: r.SatPerVbyte, Memo: r.Memo}
	var total int64
	for _, op := range r.Outpoints {
		req.Outpoints = append(req.Outpoints, &lnrpc.OutPoint{TxidStr: op.TxidStr, OutputIndex: op.OutputIndex})
		for _, u := range f.unspent {
			if u.Outpoint == op {
				total += int64(u.AmountSat)
			}
		}
	}
	p := guard.DefaultPolicy()
	p.Leases = guardLeases{f}
	p.Released = f.released
	if err := p.OpenChannelCheck(ctx, req); err != nil {
		f.t.Fatalf("the guard would refuse the worker's open: %v", err)
	}
	// As lnd: no open from coins that are leased
	for _, op := range r.Outpoints {
		if f.leases[op.String()] {
			return "", &lnd.Error{Status: 500, Message: "outpoint already spent or locked by another subsystem: " + op.String()}
		}
	}
	f.opens = append(f.opens, r)
	// The coins are spent
	var left []lnd.Utxo
	for _, u := range f.unspent {
		spent := false
		for _, op := range r.Outpoints {
			if u.Outpoint == op {
				spent = true
			}
		}
		if !spent {
			left = append(left, u)
		} else {
			delete(f.leases, u.Outpoint.String())
		}
	}
	f.unspent = left
	txid := fmt.Sprintf("%064x", 0xf00+len(f.opens))
	capacity := r.LocalFundingAmount
	if r.FundMax {
		capacity = total - int64(r.SatPerVbyte)*FundingVsize(len(r.Outpoints))
	}
	point := txid + ":0"
	f.pending = append(f.pending, lnd.PendingChannel{RemoteNodePub: r.NodePubkey,
		ChannelPoint: point, Capacity: lnd.Int(capacity), Memo: r.Memo})
	f.txs = append(f.txs, lnd.Transaction{TxHash: txid})
	if f.lostAnswer {
		return "", context.DeadlineExceeded
	}
	return point, nil
}

type guardLeases struct{ f *wallet }

func (g guardLeases) LeasedByUs(_ context.Context, txid string, index uint32) (bool, error) {
	return g.f.leases[fmt.Sprintf("%s:%d", txid, index)], nil
}

func (f *wallet) PendingOpens(context.Context) ([]lnd.PendingChannel, error) {
	return f.pending, nil
}
func (f *wallet) ListChannels(context.Context) ([]lnd.Channel, error) { return f.channels, nil }
func (f *wallet) ClosedChannels(context.Context) ([]lnd.ClosedChannel, error) {
	return f.closed, nil
}
func (f *wallet) Transactions(context.Context, int64) ([]lnd.Transaction, error) {
	return f.txs, nil
}

// pay sends amount to address; confs confirmations
func (f *wallet) pay(address string, amount, confs int64) lnd.OutPoint {
	op := lnd.OutPoint{TxidStr: fmt.Sprintf("%064x", len(f.unspent)+1), OutputIndex: 1}
	f.unspent = append(f.unspent, lnd.Utxo{Address: address, AmountSat: lnd.Int(amount),
		Outpoint: op, Confirmations: lnd.Int(confs)})
	return op
}

func (f *wallet) confirmAll(n int64) {
	for i := range f.unspent {
		f.unspent[i].Confirmations += lnd.Int(n)
	}
	f.height += n
}

// opened turns the pending channel into an open one
func (f *wallet) opened() {
	for _, p := range f.pending {
		f.channels = append(f.channels, lnd.Channel{RemotePubkey: p.RemoteNodePub,
			ChannelPoint: p.ChannelPoint, Capacity: p.Capacity, Active: true, Initiator: true})
	}
	f.pending = nil
}

type harness struct {
	t     *testing.T
	w     *Worker
	lnd   *wallet
	store *Memory
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, lnd: newWallet(t), now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	h.store = NewMemory(func() time.Time { return h.now })
	h.w = &Worker{Store: h.store, Lnd: h.lnd, Rules: DefaultRules(), Now: func() time.Time { return h.now }}
	return h
}

func (h *harness) order(node, addr string) string {
	h.t.Helper()
	id := NewID()
	_, hash := NewSecret()
	if err := h.store.CreateOrder(context.Background(), Order{ID: id, SecretHash: hash,
		NodePubkey: node, NodeAddr: addr, DisclaimerVersion: "v", ExpiresAt: h.now.Add(24 * time.Hour)}); err != nil {
		h.t.Fatal(err)
	}
	h.now = h.now.Add(time.Second)
	return id
}

func (h *harness) pass() {
	h.t.Helper()
	if err := h.w.Pass(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(id string) Order {
	o, err := h.store.GetOrder(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return o
}

func (h *harness) kinds(id string) string {
	var k []string
	for _, e := range h.store.Log[id] {
		k = append(k, e.Kind)
	}
	return strings.Join(k, " ")
}

func (h *harness) wait(d time.Duration) { h.now = h.now.Add(d) }

func TestTheWholeWay(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	o := h.get(id)
	if o.State != AwaitingPayment || o.Address == "" {
		t.Fatalf("%+v", o)
	}
	h.lnd.pay(o.Address, 2_000_000, 0)
	h.pass()
	if o = h.get(id); o.State != PaymentSeen || o.ReceivedSat != 2_000_000 || o.ConfirmedSat != 0 {
		t.Fatalf("%+v", o)
	}
	h.lnd.confirmAll(3)
	h.pass()
	o = h.get(id)
	// Confirmed, leased, connected and opened in one pass
	if o.State != FundingBroadcast || o.ChannelPoint == "" {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
	open := h.lnd.opens[0]
	if !open.FundMax || len(open.Outpoints) != 1 || open.Memo != "donation:"+id {
		t.Fatalf("%+v", open)
	}
	if o.CapacitySat != 2_000_000-2*FundingVsize(1) || o.RemainderSat != 0 {
		t.Fatalf("capacity %d remainder %d", o.CapacitySat, o.RemainderSat)
	}
	h.lnd.txs[0].NumConfirmations = 2
	h.pass()
	if o = h.get(id); o.FundingConfs != 2 {
		t.Fatalf("confs %d", o.FundingConfs)
	}
	h.lnd.opened()
	h.pass()
	if o = h.get(id); o.State != Open || o.OpenedAt == nil {
		t.Fatalf("%s", o.State)
	}
	h.lnd.closed = []lnd.ClosedChannel{{ChannelPoint: o.ChannelPoint, CloseType: "REMOTE_FORCE_CLOSE"}}
	h.pass()
	if o = h.get(id); o.State != Closed {
		t.Fatalf("%s", o.State)
	}
	want := "address payment_seen payment_confirmed connecting opening funding_broadcast " +
		"funding_confirmations open closed"
	if got := h.kinds(id); got != want {
		t.Fatalf("timeline\n got %s\nwant %s", got, want)
	}
}

func TestMoreThanTheMaximumKeepsTheRest(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 20_000_000, 3)
	h.pass()
	o := h.get(id)
	open := h.lnd.opens[0]
	if open.FundMax || open.LocalFundingAmount != 16_777_215 {
		t.Fatalf("%+v", open)
	}
	if o.RemainderSat != 20_000_000-16_777_215-2*FundingVsize(1) {
		t.Fatalf("remainder %d", o.RemainderSat)
	}
}

func TestWumboNodesMayGetTheLargerMaximum(t *testing.T) {
	h := newHarness(t)
	h.w.Rules.MaxWumboSat = 50_000_000
	h.lnd.features[donorNode] = "fork wumbo"
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 20_000_000, 3)
	h.pass()
	if !h.lnd.opens[0].FundMax || !h.get(id).Wumbo {
		t.Fatalf("%+v", h.lnd.opens[0])
	}
}

func TestTooLittleWaitsThenFallsBack(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	addr := h.get(id).Address
	h.lnd.pay(addr, 600_000, 3)
	h.pass()
	if o := h.get(id); o.State != PaymentSeen || o.ConfirmedSat != 600_000 {
		t.Fatalf("%+v", o)
	}
	// A top-up makes it enough
	h.lnd.pay(addr, 500_000, 3)
	h.pass()
	o := h.get(id)
	if o.State != FundingBroadcast || len(h.lnd.opens[0].Outpoints) != 2 {
		t.Fatalf("%s %+v", o.State, h.lnd.opens)
	}

	id = h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 300_000, 3)
	h.pass()
	h.wait(25 * time.Hour)
	h.pass()
	if o := h.get(id); o.State != FellBack || o.ErrorCode != ErrTooLittle {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
}

func TestNothingPaidExpires(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "")
	h.pass()
	h.wait(25 * time.Hour)
	h.pass()
	if o := h.get(id); o.State != Expired {
		t.Fatalf("%s", o.State)
	}
}

func TestAPaymentThatVanishesIsWaitedFor(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 1)
	h.pass()
	h.lnd.unspent = nil // replaced (RBF) or reorganised away
	h.pass()
	o := h.get(id)
	if o.State != AwaitingPayment || o.ReceivedSat != 0 || !strings.Contains(h.kinds(id), "payment_gone") {
		t.Fatalf("%s %d %s", o.State, o.ReceivedSat, h.kinds(id))
	}
}

func TestAtMostTenCoinsCount(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	addr := h.get(id).Address
	for i := 0; i < 12; i++ {
		h.lnd.pay(addr, 150_000, 3)
	}
	h.pass()
	if n := len(h.lnd.opens[0].Outpoints); n != 10 {
		t.Fatalf("%d coins in the channel", n)
	}
}

func TestUnreachableRetriesThenGivesUp(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.61:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	o := h.get(id)
	if o.State != Retrying || o.ErrorCode != ErrUnreachable || o.NextAttempt.Sub(h.now) != time.Minute {
		t.Fatalf("%s %s %v", o.State, o.ErrorCode, o.NextAttempt)
	}
	// Not before its time
	h.pass()
	if h.get(id).Attempts != 1 {
		t.Fatal("tried too soon")
	}
	for _, gap := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
		h.wait(gap)
		h.pass()
	}
	if o = h.get(id); o.Attempts != 5 || o.NextAttempt.Sub(h.now) != time.Hour {
		t.Fatalf("attempts %d next %v", o.Attempts, o.NextAttempt.Sub(h.now))
	}
	h.wait(49 * time.Hour)
	h.pass()
	o = h.get(id)
	if o.State != FellBack || o.ErrorCode != ErrGaveUp {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
	// Its coins are released to lnd's wallet: general liquidity
	if len(h.lnd.leases) != 0 {
		t.Fatalf("still leased: %v", h.lnd.leases)
	}
}

func TestAnEditRescuesAnUnreachableNode(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.61:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	_ = h.store.AddRequest(context.Background(), Request{OrderID: id, Kind: "node",
		Node: donorNode, Addr: "91.190.100.60:9735", Input: "91.190.100.60"})
	h.pass()
	o := h.get(id)
	if o.State != FundingBroadcast || o.Attempts != 0 {
		t.Fatalf("%s %d %s", o.State, o.Attempts, h.kinds(id))
	}
	if len(h.store.Queue) != 1 || h.store.Queue[0].Kind != "" {
		t.Fatal("the request was not marked done")
	}
}

func TestTheOperatorRetriesOrFallsBack(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.61:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	h.lnd.reachable["91.190.100.61:9735"] = true
	_ = h.store.AddRequest(context.Background(), Request{OrderID: id, Kind: "retry"})
	h.pass()
	if o := h.get(id); o.State != FundingBroadcast {
		t.Fatalf("%s", o.State)
	}
	delete(h.lnd.peers, donorNode) // so the next one must dial, and fails
	id = h.order(donorNode, "91.190.100.62:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	if o := h.get(id); o.State != Retrying {
		t.Fatalf("%s", o.State)
	}
	_ = h.store.AddRequest(context.Background(), Request{OrderID: id, Kind: "fallback"})
	h.pass()
	if o := h.get(id); o.State != FellBack || o.ErrorCode != "operator" || h.lnd.leases[o.Utxos[0].Outpoint] {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
}

func TestTheDonorIsAskedToActWhenOnlyTheyCan(t *testing.T) {
	cases := map[string]func(h *harness) (node, addr string){
		ErrNotForkNode: func(h *harness) (string, string) {
			h.lnd.features[donorNode] = "sha256"
			return donorNode, "91.190.100.60:9735"
		},
		ErrWrongNode: func(h *harness) (string, string) {
			h.lnd.connectErr = &lnd.Error{Status: 500, Message: "pubkey mismatch"}
			return donorNode, "91.190.100.60:9735"
		},
		ErrNoAddress: func(h *harness) (string, string) { return donorNode, "" },
		ErrOurNode:   func(h *harness) (string, string) { return ourNode, "91.190.100.60:9735" },
	}
	for code, setup := range cases {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t)
			node, addr := setup(h)
			id := h.order(node, addr)
			h.pass()
			h.lnd.pay(h.get(id).Address, 2_000_000, 3)
			h.pass()
			o := h.get(id)
			if o.State != NeedsAttention || o.ErrorCode != code {
				t.Fatalf("%s %s", o.State, o.ErrorCode)
			}
			if len(h.lnd.opens) != 0 {
				t.Fatal("opened anyway")
			}
			h.wait(8 * 24 * time.Hour)
			h.pass()
			if o = h.get(id); o.State != FellBack || o.ErrorCode != ErrNoEdit {
				t.Fatalf("%s %s", o.State, o.ErrorCode)
			}
		})
	}
}

func TestAConnectedNodeNeedsNoAddress(t *testing.T) {
	h := newHarness(t)
	h.lnd.peers[donorNode] = true
	h.lnd.connectErr = errors.New("must not dial")
	id := h.order(donorNode, "")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	if o := h.get(id); o.State != FundingBroadcast {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
}

func TestAnAddressFromTheGraph(t *testing.T) {
	h := newHarness(t)
	h.lnd.nodes[donorNode] = lnd.Node{Alias: "donor", Addresses: []lnd.NodeAddress{
		{Addr: "10.0.0.5:9735"}, {Addr: "91.190.100.60:9735"}}}
	id := h.order(donorNode, "")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	o := h.get(id)
	if o.State != FundingBroadcast || o.NodeAddr != "91.190.100.60:9735" || o.NodeAlias != "donor" {
		t.Fatalf("%s %q %q", o.State, o.NodeAddr, o.NodeAlias)
	}
}

func TestTheNodeRefusesTwiceThenTheDonorActs(t *testing.T) {
	h := newHarness(t)
	h.lnd.openErr = &lnd.Error{Status: 500, Message: "received funding error from 02...: chan size of 0.02 BTC is below min chan size of 0.05 BTC"}
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	if o := h.get(id); o.State != Retrying || o.ErrorCode != ErrRejectedSize {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
	h.wait(time.Minute)
	h.pass()
	if o := h.get(id); o.State != NeedsAttention || o.ErrorCode != ErrRejectedSize {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
	// The coins are leased again for the channel meanwhile
	if len(h.lnd.leases) != 1 {
		t.Fatalf("leases %v", h.lnd.leases)
	}
}

func TestTwoDonatedChannelsANode(t *testing.T) {
	h := newHarness(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, h.order(donorNode, "91.190.100.60:9735"))
	}
	h.pass()
	for _, id := range ids {
		h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	}
	// One open a pass
	h.pass()
	h.pass()
	h.pass()
	if len(h.lnd.opens) != 2 {
		t.Fatalf("%d opens", len(h.lnd.opens))
	}
	if o := h.get(ids[2]); o.State != NeedsAttention || o.ErrorCode != ErrNodeLimit {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
}

func TestAnOpenWhoseAnswerIsLostIsAdopted(t *testing.T) {
	h := newHarness(t)
	h.lnd.lostAnswer = true
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	o := h.get(id)
	if o.State != FundingBroadcast || o.ChannelPoint != h.lnd.pending[0].ChannelPoint {
		t.Fatalf("%s %q %s", o.State, o.ChannelPoint, h.kinds(id))
	}
	// Not leased again: the coins are spent
	if len(h.lnd.leases) != 0 {
		t.Fatalf("leases %v", h.lnd.leases)
	}
	// Another order's channel to the same node is not taken
	h.lnd.lostAnswer = false
	other := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(other).Address, 2_000_000, 3)
	h.lnd.openErr = errors.New("refused")
	h.pass()
	if o := h.get(other); o.State != Retrying || o.ChannelPoint != "" {
		t.Fatalf("%s %q", o.State, o.ChannelPoint)
	}
}

func TestAddressesAreCounted(t *testing.T) {
	h := newHarness(t)
	h.order(donorNode, "")
	h.order(donorNode, "")
	h.pass()
	if v := h.store.Settings[SettingAddressesIssued]; v != "2" {
		t.Fatalf("%q", v)
	}
}

func TestOneOpenAPass(t *testing.T) {
	h := newHarness(t)
	a := h.order(donorNode, "91.190.100.60:9735")
	b := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(a).Address, 2_000_000, 3)
	h.lnd.pay(h.get(b).Address, 2_000_000, 3)
	h.pass()
	if len(h.lnd.opens) != 1 || h.get(b).State != PaymentConfirmed {
		t.Fatalf("%d opens, b %s", len(h.lnd.opens), h.get(b).State)
	}
	h.pass()
	if len(h.lnd.opens) != 2 {
		t.Fatal("b not opened next")
	}
}

func TestAnInterruptedOpenIsResumed(t *testing.T) {
	// Stopped before lnd spent anything: tried again
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.lnd.openErr = errors.New("worker killed")
	h.pass()
	o := h.get(id)
	o.State = Opening // as the crash left it
	_ = h.store.Save(context.Background(), o)
	h.lnd.openErr = nil
	h.pass()
	h.pass()
	if o = h.get(id); o.State != FundingBroadcast || len(h.lnd.opens) != 1 {
		t.Fatalf("%s %d", o.State, len(h.lnd.opens))
	}

	// Stopped after lnd published the funding: the channel is adopted
	h = newHarness(t)
	id = h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	o = h.get(id)
	point := o.ChannelPoint
	o.State, o.ChannelPoint = Opening, ""
	_ = h.store.Save(context.Background(), o)
	h.pass()
	if o = h.get(id); o.State != FundingBroadcast || o.ChannelPoint != point || len(h.lnd.opens) != 1 {
		t.Fatalf("%s %s %d", o.State, o.ChannelPoint, len(h.lnd.opens))
	}
}

func TestCoinsLeftUnleasedAfterAFailedOpenAreLeasedAgain(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.pass()
	if h.get(id).State != FundingBroadcast {
		t.Fatal("the usual open failed")
	}
	// Another order: its open fails and the coins cannot be leased again
	id = h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	h.lnd.pay(h.get(id).Address, 2_000_000, 3)
	h.lnd.openErr = errors.New("lnd restarted")
	failing := &failingLease{wallet: h.lnd, after: 1, calls: new(int)}
	h.w.Lnd = failing
	h.pass()
	o := h.get(id)
	if o.State != Retrying || h.lnd.leases[o.Utxos[0].Outpoint] {
		t.Fatalf("%s %v", o.State, h.lnd.leases)
	}
	h.w.Lnd = h.lnd
	h.lnd.openErr = nil
	h.wait(time.Minute)
	h.pass()
	if o = h.get(id); o.State != FundingBroadcast || o.ErrorCode != "" {
		t.Fatalf("%s %s", o.State, o.ErrorCode)
	}
}

func TestALeaseThatFailsHalfwayIsUndone(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "91.190.100.60:9735")
	h.pass()
	addr := h.get(id).Address
	h.lnd.pay(addr, 600_000, 3)
	h.lnd.pay(addr, 600_000, 3)
	calls := 0
	h.lnd.leaseErr = nil
	orig := h.lnd
	failing := &failingLease{wallet: orig, after: 1, calls: &calls}
	h.w.Lnd = failing
	_ = h.w.Pass(context.Background())
	if len(orig.leases) != 0 {
		t.Fatalf("a half lease was left: %v", orig.leases)
	}
	h.w.Lnd = orig
	h.pass()
	if o := h.get(id); o.State != FundingBroadcast {
		t.Fatalf("%s", o.State)
	}
}

type failingLease struct {
	*wallet
	after int
	calls *int
}

func (f *failingLease) LeaseOutput(ctx context.Context, id []byte, op lnd.OutPoint, s uint64) error {
	*f.calls++
	if *f.calls > f.after {
		return errors.New("lnd hiccup")
	}
	return f.wallet.LeaseOutput(ctx, id, op, s)
}

func TestTheBudgetOfUnpaidOrders(t *testing.T) {
	h := newHarness(t)
	h.w.Rules.MaxUnpaid = 2
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, h.order(donorNode, ""))
	}
	h.pass()
	if o := h.get(ids[2]); o.State != Rejected || o.ErrorCode != ErrBudget {
		t.Fatalf("%s", o.State)
	}
	if h.lnd.addresses != 2 {
		t.Fatalf("%d addresses handed out", h.lnd.addresses)
	}
}

func TestPublishedNumbersAndLndDown(t *testing.T) {
	h := newHarness(t)
	h.lnd.fee = 7
	h.pass()
	if v := h.store.Settings[SettingFeeRate]; v != "7" {
		t.Fatalf("fee %s", v)
	}
	if v := h.store.Settings[SettingMinSat]; v != fmt.Sprint(1_000_000+7*FundingVsize(1)) {
		t.Fatalf("min %s", v)
	}
	h.lnd.fee = 500
	h.pass()
	if v := h.store.Settings[SettingFeeRate]; v != "100" {
		t.Fatalf("not capped: %s", v)
	}
	h.lnd.down = true
	if err := h.w.Pass(context.Background()); err == nil {
		t.Fatal("no error with lnd down")
	}
}

func TestFinalOrdersArePruned(t *testing.T) {
	h := newHarness(t)
	id := h.order(donorNode, "")
	h.pass()
	h.wait(25 * time.Hour)
	h.pass()
	h.wait(91 * 24 * time.Hour)
	h.pass()
	if _, err := h.store.GetOrder(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kept: %v", err)
	}
}

func TestClassify(t *testing.T) {
	for msg, want := range map[string]string{
		"dial tcp 1.2.3.4:9735: i/o timeout":                 ErrUnreachable,
		"dial tcp 1.2.3.4:9735: connect: connection refused": ErrUnreachable,
		"pubkey mismatch": ErrWrongNode,
		"chan size of 0.01 BTC is below min chan size of 0.1":     ErrRejectedSize,
		"pending channels exceed maximum":                         ErrPending,
		"peer 02ab is not online":                                 ErrDisconnected,
		"received funding error from 02ab: channel open canceled": ErrRejected,
		"refused by the donations guard: push_sat must be 0":      ErrInternal,
		"something else": ErrInternal,
	} {
		if got := classify(&lnd.Error{Status: 500, Message: msg}); got != want {
			t.Errorf("%q: %s, want %s", msg, got, want)
		}
	}
}
