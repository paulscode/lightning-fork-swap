package sha256don

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/knots"
)

// explorer is a fake Esplora: transactions by address, raw bytes by txid
type explorer struct {
	byAddress map[string][]esplora.Tx
	txs       map[string]*esplora.Tx
	hex       map[string]string
	tip       int64
	fail      error
}

func newExplorer() *explorer {
	return &explorer{byAddress: map[string][]esplora.Tx{}, txs: map[string]*esplora.Tx{},
		hex: map[string]string{}, tip: 100}
}

func (e *explorer) AddressTxs(_ context.Context, a string) ([]esplora.Tx, error) {
	return e.byAddress[a], e.fail
}
func (e *explorer) Tx(_ context.Context, txid string) (*esplora.Tx, error) {
	if tx, ok := e.txs[txid]; ok {
		return tx, nil
	}
	return nil, esplora.ErrNotFound
}
func (e *explorer) TxHex(_ context.Context, txid string) (string, error) {
	if h, ok := e.hex[txid]; ok {
		return h, nil
	}
	return "", esplora.ErrNotFound
}
func (e *explorer) TxStatus(_ context.Context, txid string) (*esplora.Status, error) {
	if tx, ok := e.txs[txid]; ok {
		return &tx.Status, nil
	}
	return nil, esplora.ErrNotFound
}
func (e *explorer) TipHeight(context.Context) (int64, error) { return e.tip, e.fail }

// pay makes a payment of value to address, from an input of from
func (e *explorer) pay(txid, address string, value, height int64, from string) {
	tx := esplora.Tx{Txid: txid,
		Vin:    []esplora.Vin{{Txid: strings.Repeat("9", 64), Prevout: &esplora.Prevout{Address: from}}},
		Vout:   []esplora.Vout{{Address: "bc1qchange", Value: 5}, {Address: address, Value: value}},
		Status: esplora.Status{Confirmed: height > 0, BlockHeight: height}}
	e.byAddress[address] = append(e.byAddress[address], tx)
	e.txs[txid] = &tx
}

// node is a fake Knots
type node struct {
	utxos  map[string]*knots.TxOut
	refuse map[string]string // raw -> reason
	sent   []string
	height int64
}

func newNode() *node {
	return &node{utxos: map[string]*knots.TxOut{}, refuse: map[string]string{}, height: 1000}
}

func (n *node) TxOut(_ context.Context, txid string, vout uint32, _ bool) (*knots.TxOut, error) {
	return n.utxos[fmt.Sprintf("%s:%d", txid, vout)], nil
}
func (n *node) TestAccept(_ context.Context, raw string) (knots.Accept, error) {
	if reason, ok := n.refuse[raw]; ok {
		return knots.Accept{Reason: reason}, nil
	}
	for r, reason := range n.refuse {
		if r == "*" {
			return knots.Accept{Reason: reason}, nil
		}
	}
	return knots.Accept{Allowed: true}, nil
}
func (n *node) Send(_ context.Context, raw string) (string, error) {
	n.sent = append(n.sent, raw)
	// Into the mempool: its outputs are visible, unconfirmed
	if txid, err := txidOf(raw); err == nil {
		if _, ok := n.utxos[txid+":0"]; !ok {
			n.utxos[txid+":0"] = &knots.TxOut{}
		}
	}
	return txid(raw), nil
}

func txid(raw string) string                                  { id, _ := txidOf(raw); return id }
func (n *node) FeeRate(context.Context, int) (float64, error) { return 2, nil }
func (n *node) Height(context.Context) (int64, error)         { return n.height, nil }

// put makes our node hold an output at one of our addresses
func (n *node) put(t *testing.T, k *Keys, index uint32, txid string, vout uint32, value, confs int64) {
	t.Helper()
	_, script, err := k.Address(index)
	if err != nil {
		t.Fatal(err)
	}
	n.utxos[fmt.Sprintf("%s:%d", txid, vout)] = &knots.TxOut{ValueSat: value,
		Confirmations: confs, ScriptPubKey: fmt.Sprintf("%x", script)}
}

const lndAddress = "bc1p0ckyk3hjmft4lvwqhrd3c02uwc8rt3pkhy698yjwz9jdeggh3s0qju3dgm"

type harness struct {
	w     *Worker
	store *Memory
	sha   *explorer
	blake *explorer
	node  *node
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{store: NewMemory(), sha: newExplorer(), blake: newExplorer(),
		node: newNode(), clock: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)}
	h.store.Now = func() time.Time { return h.clock }
	h.w = &Worker{Store: h.store, Keys: vectorKeys(t), Params: &chaincfg.MainNetParams,
		Sha256: h.sha, Blake2b: h.blake, Node: h.node, LndAddress: lndAddress,
		Now: func() time.Time { return h.clock }, Logger: log.New(io.Discard, "", 0),
		Verify: func(address, message, signature string) error {
			if signature != "signed:"+address+":"+message {
				return errors.New("bad proof")
			}
			return nil
		}}
	return h
}

func (h *harness) pass(t *testing.T) {
	t.Helper()
	h.clock = h.clock.Add(time.Minute)
	if err := h.w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) request(t *testing.T, id, ret string, replay bool) Request {
	t.Helper()
	if err := h.store.CreateRequest(context.Background(), Request{ID: id, ReturnAddress: ret, Replay: replay}); err != nil {
		t.Fatal(err)
	}
	h.pass(t)
	r, err := h.store.GetRequest(context.Background(), id)
	if err != nil || r.Address == "" {
		t.Fatalf("no address: %+v %v", r, err)
	}
	return r
}

func (h *harness) deposits(t *testing.T, id, chain string) []Deposit {
	ds, _ := h.store.Deposits(context.Background(), id)
	var out []Deposit
	for _, d := range ds {
		if d.Chain == chain {
			out = append(out, d)
		}
	}
	return out
}

var (
	txA = strings.Repeat("a", 64)
	txB = strings.Repeat("b", 64)
)

func TestADonationCopiedByItselfIsPassedOn(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	if r.Address != "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr" {
		t.Fatalf("first address %s", r.Address)
	}

	// Paid on the SHA256 chain, copied to the BLAKE2b chain by the network
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 1)
	h.pass(t)
	sha := h.deposits(t, r.ID, SHA256)
	if len(sha) != 1 || sha[0].State != Confirmed || sha[0].Confirmations != 3 {
		t.Fatalf("SHA256: %+v", sha)
	}
	blake := h.deposits(t, r.ID, BLAKE2b)
	if len(blake) != 1 || blake[0].State != Seen || blake[0].ValueSat != 50_000 {
		t.Fatalf("BLAKE2b: %+v", blake)
	}
	if len(h.node.sent) != 0 {
		t.Fatal("passed on before it was deep enough")
	}

	// Deep enough: passed on to the return address, unified
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 6)
	h.pass(t)
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
	}
	tx := decode(t, h.node.sent[0])
	if err := CheckUnified(tx, fetcherFor(t, h.w.Keys, []Coin{{Txid: txA, Vout: 1, ValueSat: 50_000}})); err != nil {
		t.Fatal(err)
	}
	fwd, _ := h.store.ForwardsIn(context.Background(), Sent)
	if len(fwd) != 1 || fwd[0].Dest != dest || fwd[0].AmountSat+fwd[0].FeeSat != 50_000 {
		t.Fatalf("forward: %+v", fwd)
	}
	// The deposit is spent by the forward; it is not "gone"
	delete(h.node.utxos, txA+":1")
	h.node.utxos[fwd[0].Txid+":0"].Confirmations = 2
	h.pass(t)
	if b := h.deposits(t, r.ID, BLAKE2b); b[0].State != Forwarding {
		t.Fatalf("while confirming: %+v", b)
	}
	h.node.utxos[fwd[0].Txid+":0"].Confirmations = 6
	h.pass(t)
	if b := h.deposits(t, r.ID, BLAKE2b); b[0].State != Forwarded {
		t.Fatalf("after: %+v", b)
	}
	if len(h.node.sent) != 1 {
		t.Fatal("passed on twice")
	}
}

func TestWithoutAReturnAddressTheCoinsGoToLnd(t *testing.T) {
	h := newHarness(t)
	r1 := h.request(t, "req-1", "", false)
	r2 := h.request(t, "req-2", "", false)
	h.node.put(t, h.w.Keys, 0, txA, 0, 30_000, 6)
	h.node.put(t, h.w.Keys, 1, txB, 3, 20_000, 7)
	// Plain BLAKE2b sends, found through the explorer
	h.blake.pay(txA, r1.Address, 30_000, 1, "bc1qx")
	h.blake.byAddress[r1.Address][0].Vout = []esplora.Vout{{Address: r1.Address, Value: 30_000}}
	h.blake.byAddress[r2.Address] = []esplora.Tx{{Txid: txB, Vout: []esplora.Vout{
		{}, {}, {}, {Address: r2.Address, Value: 20_000}}}}
	h.pass(t)
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
	}
	fwd, _ := h.store.ForwardsIn(context.Background(), Sent)
	if len(fwd) != 1 || fwd[0].Dest != lndAddress || fwd[0].RequestID != "" ||
		fwd[0].AmountSat+fwd[0].FeeSat != 50_000 {
		t.Fatalf("one batch to lnd: %+v", fwd)
	}
	if tx := decode(t, h.node.sent[0]); len(tx.TxIn) != 2 {
		t.Fatalf("inputs %d", len(tx.TxIn))
	}
}

func TestACopyNeedsProofsFromEveryInput(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, true)
	h.sha.pay(txA, r.Address, 50_000, 99, "bc1qdonor")
	h.sha.hex[txA] = "not checked yet"
	h.pass(t)
	d := h.deposits(t, r.ID, SHA256)[0]
	if d.CopyState != CopyNoProof || !strings.Contains(d.CopyReason, "bc1qdonor") {
		t.Fatalf("without proofs: %+v", d)
	}

	// A proof from the input's address; the explorer's bytes must be
	// the transaction asked about
	msg := ProofMessage(r.ID, txA, dest)
	_ = h.store.AddHint(context.Background(), Hint{RequestID: r.ID, Txid: txA,
		Proofs: []Proof{{Address: "bc1qdonor", Signature: "signed:bc1qdonor:" + msg}}})
	h.pass(t)
	if d := h.deposits(t, r.ID, SHA256)[0]; d.CopyState != CopyNoProof || len(h.node.sent) != 0 {
		t.Fatalf("other bytes were relayed: %+v", d)
	}
	signed, err := Sign(h.w.Keys, coins(), dest, 2, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	// Our test transaction stands in for the donation
	h.sha.txs[signed.Txid] = h.sha.txs[txA]
	h.sha.txs[signed.Txid].Txid = signed.Txid
	h.sha.byAddress[r.Address][0].Txid = signed.Txid
	h.sha.hex[signed.Txid] = signed.Raw
	msg = ProofMessage(r.ID, signed.Txid, dest)
	_ = h.store.AddHint(context.Background(), Hint{RequestID: r.ID, Txid: signed.Txid,
		Proofs: []Proof{{Address: "bc1qdonor", Signature: "signed:bc1qdonor:" + msg}}})
	h.pass(t)
	var copied *Deposit
	for _, d := range h.deposits(t, r.ID, SHA256) {
		if d.Txid == signed.Txid {
			copied = &d
		}
	}
	if copied == nil || copied.CopyState != CopyDone || len(h.node.sent) != 1 || h.node.sent[0] != signed.Raw {
		t.Fatalf("copy: %+v sent %d", copied, len(h.node.sent))
	}
}

func TestACopyAlreadyThereIsNotRelayed(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, true)
	h.sha.pay(txA, r.Address, 50_000, 99, "bc1qdonor")
	// In our node's mempool already: no proof needed, nothing relayed
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 0)
	h.pass(t)
	d := h.deposits(t, r.ID, SHA256)[0]
	if d.CopyState != CopyAlready || len(h.node.sent) != 0 {
		t.Fatalf("%+v sent %d", d, len(h.node.sent))
	}
}

func TestACopyTheChainRefusesIsExplained(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, true)
	signed, _ := Sign(h.w.Keys, coins(), dest, 2, &chaincfg.MainNetParams)
	h.sha.pay(signed.Txid, r.Address, 50_000, 99, "bc1qdonor")
	h.sha.hex[signed.Txid] = signed.Raw
	msg := ProofMessage(r.ID, signed.Txid, dest)
	_ = h.store.AddHint(context.Background(), Hint{RequestID: r.ID, Txid: signed.Txid,
		Proofs: []Proof{{Address: "bc1qdonor", Signature: "signed:bc1qdonor:" + msg}}})
	h.node.refuse[signed.Raw] = "bad-txns-inputs-missingorspent"
	h.pass(t)
	d := h.deposits(t, r.ID, SHA256)[0]
	if d.CopyState != CopyNotPossible || !strings.Contains(d.CopyReason, "never earlier transactions") {
		t.Fatalf("%+v", d)
	}
	if len(h.node.sent) != 0 {
		t.Fatal("relayed a refused copy")
	}
}

func TestAForwardOurNodeRefusesReleasesItsCoins(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.blake.byAddress[r.Address] = []esplora.Tx{{Txid: txA, Vout: []esplora.Vout{{Address: r.Address, Value: 40_000}}}}
	h.node.put(t, h.w.Keys, 0, txA, 0, 40_000, 9)
	h.node.refuse["*"] = "min relay fee not met"
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("relayed what the node refused")
	}
	if d := h.deposits(t, r.ID, BLAKE2b)[0]; d.State != Ready {
		t.Fatalf("coins not released: %+v", d)
	}
	failed, _ := h.store.ForwardsIn(context.Background(), FailedSend)
	if len(failed) != 1 || failed[0].Error != "min relay fee not met" {
		t.Fatalf("%+v", failed)
	}
	// Next time it goes through
	delete(h.node.refuse, "*")
	h.pass(t)
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
	}
}

func TestDustWaitsAndALostCoinIsGone(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.blake.byAddress[r.Address] = []esplora.Tx{{Txid: txA, Vout: []esplora.Vout{{Address: r.Address, Value: 900}}},
		{Txid: txB, Vout: []esplora.Vout{{Address: r.Address, Value: 5000}}}}
	h.node.put(t, h.w.Keys, 0, txA, 0, 900, 8)
	h.node.put(t, h.w.Keys, 0, txB, 0, 5000, 1)
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("dust was passed on")
	}
	// The unconfirmed one leaves the chain (replaced)
	delete(h.node.utxos, txB+":0")
	for i := 0; i < missingLimit; i++ {
		h.pass(t)
	}
	for _, d := range h.deposits(t, r.ID, BLAKE2b) {
		want := Ready
		if d.Txid == txB {
			want = Gone
		}
		if d.State != want {
			t.Fatalf("%s: %s, want %s", d.Txid[:4], d.State, want)
		}
	}
}

func TestOldRequestsAreLookedAtLessOften(t *testing.T) {
	for age, want := range map[time.Duration]time.Duration{
		time.Hour: 0, 3 * 24 * time.Hour: 10 * time.Minute,
		20 * 24 * time.Hour: time.Hour, 100 * 24 * time.Hour: 24 * time.Hour,
		400 * 24 * time.Hour: -1} {
		if got := PollEvery(age); got != want {
			t.Fatalf("%v: %v", age, got)
		}
	}
}
