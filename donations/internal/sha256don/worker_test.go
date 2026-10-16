package sha256don

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/knots"
)

// explorer is a fake Esplora: transactions by address, raw bytes by txid
type explorer struct {
	byAddress map[string][]esplora.Tx
	txs       map[string]*esplora.Tx
	hex       map[string]string
	spent     map[string]string // outpoint -> spender
	tip       int64
	fail      error
	feeRate   float64
	broadcast []string
	refuse    error
	onRefuse  func(raw string)
	failFor   map[string]bool // addresses whose listing fails
}

func newExplorer() *explorer {
	return &explorer{byAddress: map[string][]esplora.Tx{}, txs: map[string]*esplora.Tx{},
		hex: map[string]string{}, spent: map[string]string{}, tip: 100, feeRate: 4}
}

func (e *explorer) AddressTxs(_ context.Context, a string) ([]esplora.Tx, error) {
	if e.failFor[a] {
		return nil, errors.New("explorers: too large")
	}
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
func (e *explorer) Outspend(_ context.Context, txid string, vout uint32) (*esplora.Outspend, error) {
	if e.fail != nil {
		return nil, e.fail
	}
	if by, ok := e.spent[fmt.Sprintf("%s:%d", txid, vout)]; ok {
		o := &esplora.Outspend{Spent: true, Txid: by}
		if tx, ok := e.txs[by]; ok {
			st := tx.Status
			o.Status = &st
		}
		return o, nil
	}
	return &esplora.Outspend{}, nil
}
func (e *explorer) FeeRate(context.Context, int) (float64, error) { return e.feeRate, e.fail }
func (e *explorer) Broadcast(_ context.Context, raw string) (string, error) {
	if e.refuse != nil {
		if e.onRefuse != nil {
			e.onRefuse(raw)
		}
		return "", e.refuse
	}
	e.broadcast = append(e.broadcast, raw)
	id := txid(raw)
	e.txs[id] = &esplora.Tx{Txid: id}
	// What it spends is spent, by it
	if tx, err := rawTx(raw); err == nil {
		for _, in := range tx.TxIn {
			e.spent[in.PreviousOutPoint.String()] = id
		}
	}
	return id, nil
}

// pay makes a payment of value to address, from an input of from
func (e *explorer) pay(txid, address string, value, height int64, from string) {
	tx := esplora.Tx{Txid: txid,
		Vin:    []esplora.Vin{{Txid: strings.Repeat("9", 64), Prevout: &esplora.Prevout{Address: from}}},
		Vout:   []esplora.Vout{{Address: "bc1qchange", Value: 5}, {Address: address, Value: value}},
		Status: esplora.Status{Confirmed: height > 0, BlockHeight: height}}
	e.byAddress[address] = append(e.byAddress[address], tx)
	e.txs[txid] = &tx
}

// confirm puts a transaction in a block at height
func (e *explorer) confirm(txid string, height int64) {
	e.txs[txid].Status = esplora.Status{Confirmed: true, BlockHeight: height}
}

// sendProtected makes a plain BLAKE2b send to address (output vout), from
// a sender's coin, really signed with the unified hash type: it can never
// be valid on the SHA256 chain as signed. It returns its txid.
func (e *explorer) sendProtected(t *testing.T, address string, vout int, value int64, n byte) string {
	t.Helper()
	sender := senderKeys(t)
	from := Coin{Txid: fmt.Sprintf("%064x", 0x8000+int(n)), Vout: 0, ValueSat: value + 1000, KeyIndex: uint32(n)}
	s, err := Sign(sender, []Coin{from}, address, 1000.0/200, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	// Put the payment at output vout, signed again
	msg, _ := rawTx(s.Raw)
	pay := msg.TxOut[0]
	pay.Value = value
	msg.TxOut = make([]*wire.TxOut, vout+1)
	for i := range msg.TxOut {
		msg.TxOut[i] = wire.NewTxOut(0, []byte{txscript.OP_RETURN})
	}
	msg.TxOut[vout] = pay
	_, script, _ := sender.Address(uint32(n))
	f := txscript.NewMultiPrevOutFetcher(nil)
	f.AddPrevOut(msg.TxIn[0].PreviousOutPoint, wire.NewTxOut(from.ValueSat, script))
	priv, _ := sender.Private(uint32(n))
	w, err := txscript.TaprootWitnessSignature(msg, txscript.NewTxSigHashes(msg, f), 0,
		from.ValueSat, script, HashType, priv)
	if err != nil {
		t.Fatal(err)
	}
	msg.TxIn[0].Witness = w
	var buf bytes.Buffer
	_ = msg.Serialize(&buf)
	id := msg.TxHash().String()
	outs := make([]esplora.Vout, vout+1)
	outs[vout] = esplora.Vout{Address: address, Value: value}
	tx := esplora.Tx{Txid: id, Vout: outs, Status: esplora.Status{Confirmed: true, BlockHeight: 990_000},
		Vin: []esplora.Vin{{Txid: from.Txid, Vout: 0, Witness: []string{hex.EncodeToString(w[0])},
			Prevout: &esplora.Prevout{Type: "v1_p2tr", Address: "bc1psender", Value: from.ValueSat,
				ScriptPubKey: hex.EncodeToString(script)}}}}
	e.byAddress[address] = append(e.byAddress[address], tx)
	e.txs[id] = &tx
	e.hex[id] = hex.EncodeToString(buf.Bytes())
	return id
}

// senderKeys: someone else's keys
func senderKeys(t *testing.T) *Keys {
	k, err := NewKeys(bytes.Repeat([]byte{9}, 32), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// node is a fake Knots of the BLAKE2b chain: it refuses a transaction with
// an output its data rules refuse, and one spending such a transaction's
// outputs (which it never has)
type node struct {
	utxos    map[string]*knots.TxOut
	refuse   map[string]string // raw (or "*") -> reason
	allowAll bool
	rdts     map[string]bool
	sent     []string
	height   int64
}

func newNode() *node {
	return &node{utxos: map[string]*knots.TxOut{}, refuse: map[string]string{},
		rdts: map[string]bool{}, height: 1000}
}

func (n *node) TxOut(_ context.Context, txid string, vout uint32, _ bool) (*knots.TxOut, error) {
	return n.utxos[fmt.Sprintf("%s:%d", txid, vout)], nil
}
func (n *node) TestAccept(_ context.Context, raw string) (knots.Accept, error) {
	if reason, ok := n.refuse[raw]; ok {
		return knots.Accept{Reason: reason}, nil
	}
	if reason, ok := n.refuse["*"]; ok {
		return knots.Accept{Reason: reason}, nil
	}
	if n.allowAll {
		return knots.Accept{Allowed: true}, nil
	}
	if tx, err := rawTx(raw); err == nil {
		for _, in := range tx.TxIn {
			if n.rdts[in.PreviousOutPoint.Hash.String()] {
				return knots.Accept{Reason: "bad-txns-inputs-missingorspent"}, nil
			}
		}
		if BreaksRDTS(tx) {
			n.rdts[tx.TxHash().String()] = true
			return knots.Accept{Reason: "bad-txns-vout-script-toolarge"}, nil
		}
	}
	return knots.Accept{Allowed: true}, nil
}

func rawTx(raw string) (*wire.MsgTx, error) {
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	tx := wire.NewMsgTx(2)
	return tx, tx.Deserialize(bytes.NewReader(b))
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

func TestADonationCopiedByItselfIsLockedThenPassedOn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r := h.request(t, "req-1", dest, false)
	if r.Address != "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr" {
		t.Fatalf("first address %s", r.Address)
	}

	// Paid on the SHA256 chain (3 deep), copied to the BLAKE2b chain by the
	// network: the SHA256 coin is locked at once
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 1)
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatalf("locks relayed %d", len(h.sha.broadcast))
	}
	lock := decode(t, h.sha.broadcast[0])
	if len(lock.TxIn) != 1 || lock.TxIn[0].PreviousOutPoint.String() != txA+":1" || !BreaksRDTS(lock) {
		t.Fatalf("lock %+v", lock)
	}
	if sha := h.deposits(t, r.ID, SHA256); len(sha) != 1 || sha[0].State != Locking {
		t.Fatalf("SHA256: %+v", sha)
	}

	// The BLAKE2b twin deep enough, but the lock is not: held
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 6)
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("passed on before the lock was deep")
	}
	if b := h.deposits(t, r.ID, BLAKE2b); b[0].State != Ready || !strings.Contains(b[0].Hold, "locked") {
		t.Fatalf("held: %+v", b)
	}

	// The lock deep enough: passed on, unified, in the same pass
	h.sha.confirm(lock.TxHash().String(), 97) // 4 deep: not enough
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("passed on with the lock 4 deep")
	}
	h.sha.confirm(lock.TxHash().String(), 95) // 6 deep
	h.pass(t)
	if sha := h.deposits(t, r.ID, SHA256); sha[0].State != Locked {
		t.Fatalf("not locked: %+v", sha)
	}
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
	}
	tx := decode(t, h.node.sent[0])
	if err := CheckUnified(tx, fetcherFor(t, h.w.Keys, []Coin{{Txid: txA, Vout: 1, ValueSat: 50_000}})); err != nil {
		t.Fatal(err)
	}
	if b := h.deposits(t, r.ID, BLAKE2b); b[0].Hold != "" {
		t.Fatalf("hold left: %q", b[0].Hold)
	}
	fwd, _ := h.store.ForwardsIn(ctx, Sent)
	if len(fwd) != 1 || fwd[0].Dest != dest || fwd[0].AmountSat+fwd[0].FeeSat != 50_000 {
		t.Fatalf("forward: %+v", fwd)
	}
	// The deposit is spent by the forward; it is not "gone"
	delete(h.node.utxos, txA+":1")
	h.node.utxos[fwd[0].Txid+":0"].Confirmations = 6
	h.pass(t)
	if b := h.deposits(t, r.ID, BLAKE2b); b[0].State != Forwarded {
		t.Fatalf("after: %+v", b)
	}
	if len(h.node.sent) != 1 || len(h.sha.broadcast) != 1 {
		t.Fatal("passed on or locked twice")
	}
}

func TestWithoutAReturnAddressTheCoinsGoToLnd(t *testing.T) {
	h := newHarness(t)
	r1 := h.request(t, "req-1", "", false)
	r2 := h.request(t, "req-2", "", false)
	// Plain BLAKE2b sends, replay-protected, found through the explorer
	a := h.blake.sendProtected(t, r1.Address, 0, 30_000, 1)
	b := h.blake.sendProtected(t, r2.Address, 3, 20_000, 2)
	h.node.put(t, h.w.Keys, 0, a, 0, 30_000, 6)
	h.node.put(t, h.w.Keys, 1, b, 3, 20_000, 7)
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

func TestCoinsThatCouldStillAppearOnTheSHA256ChainAreHeld(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	// A plain BLAKE2b send signed the ordinary way, from a coin made
	// before the fork: it could still be copied to the SHA256 chain
	parent := strings.Repeat("6", 64)
	h.blake.txs[parent] = &esplora.Tx{Txid: parent, Status: esplora.Status{Confirmed: true, BlockHeight: 900_000}}
	tx := esplora.Tx{Txid: txA, Vout: []esplora.Vout{{Address: r.Address, Value: 40_000}},
		Vin: []esplora.Vin{{Txid: parent, Witness: []string{strings.Repeat("11", 64)},
			Prevout: &esplora.Prevout{Type: "v1_p2tr", Address: "bc1psender"}}}}
	h.blake.byAddress[r.Address] = []esplora.Tx{tx}
	h.blake.txs[txA] = &tx
	h.node.put(t, h.w.Keys, 0, txA, 0, 40_000, 9)
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("passed on coins that could still appear on the SHA256 chain")
	}
	d := h.deposits(t, r.ID, BLAKE2b)[0]
	if !strings.Contains(d.Hold, "could still appear on the SHA256 chain") {
		t.Fatalf("hold %q", d.Hold)
	}
	// The donor moves those coins on the SHA256 chain: then they can't
	h.sha.spent[parent+":0"] = strings.Repeat("5", 64)
	h.sha.txs[strings.Repeat("5", 64)] = &esplora.Tx{Txid: strings.Repeat("5", 64),
		Status: esplora.Status{Confirmed: true, BlockHeight: h.sha.tip - 10}}
	h.clock = h.clock.Add(10 * time.Minute)
	h.pass(t)
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d after the twin moved", len(h.node.sent))
	}
}

// donorTx: a SHA256-chain donation of the sender's two coins (keys 1 and
// 2 of senderKeys), signed the ordinary way, paying address 50,000 at
// output 0; put in the SHA256 explorer at height, with its real spent
// outputs. It returns the txid, the raw bytes and the inputs' addresses.
func donorTx(t *testing.T, e *explorer, address string, height int64) (string, string, []string) {
	t.Helper()
	sender := senderKeys(t)
	ins := []Coin{{Txid: strings.Repeat("7", 64), Vout: 0, ValueSat: 30_000, KeyIndex: 1},
		{Txid: strings.Repeat("7", 64), Vout: 1, ValueSat: 30_000, KeyIndex: 2}}
	s, err := SignSHA256(sender, ins, address, 2, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	tx := esplora.Tx{Txid: s.Txid, Status: esplora.Status{Confirmed: height > 0, BlockHeight: height},
		Vout: []esplora.Vout{{Address: address, Value: s.AmountSat}}}
	var addrs []string
	msg, _ := rawTx(s.Raw)
	for i, c := range ins {
		a, script, _ := sender.Address(c.KeyIndex)
		addrs = append(addrs, a)
		tx.Vin = append(tx.Vin, esplora.Vin{Txid: c.Txid, Vout: c.Vout,
			Witness: []string{hex.EncodeToString(msg.TxIn[i].Witness[0])},
			Prevout: &esplora.Prevout{Address: a, Type: "v1_p2tr", Value: c.ValueSat,
				ScriptPubKey: hex.EncodeToString(script)}})
	}
	e.byAddress[address] = append(e.byAddress[address], tx)
	e.txs[s.Txid] = &tx
	e.hex[s.Txid] = s.Raw
	return s.Txid, s.Raw, addrs
}

func prove(h *harness, r Request, txid string, addrs ...string) {
	msg := ProofMessage(r.ID, txid, dest)
	var proofs []Proof
	for _, a := range addrs {
		proofs = append(proofs, Proof{Address: a, Signature: "signed:" + a + ":" + msg})
	}
	_ = h.store.AddHint(context.Background(), Hint{RequestID: r.ID, Txid: txid, Proofs: proofs})
}

func TestACopyNeedsProofsFromEveryInput(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, true)
	id, raw, addrs := donorTx(t, h.sha, r.Address, 99)
	h.pass(t)
	d := h.deposits(t, r.ID, SHA256)[0]
	if d.CopyState != CopyNoProof || !strings.Contains(d.CopyReason, addrs[0]) || !strings.Contains(d.CopyReason, addrs[1]) {
		t.Fatalf("without proofs: %+v", d)
	}
	// One of the two inputs' owners only: not enough
	prove(h, r, id, addrs[0])
	h.pass(t)
	if d := h.deposits(t, r.ID, SHA256)[0]; d.CopyState != CopyNoProof || len(h.node.sent) != 0 {
		t.Fatalf("copied with one proof of two: %+v", d)
	}
	prove(h, r, id, addrs[1])
	h.pass(t)
	if d := h.deposits(t, r.ID, SHA256)[0]; d.CopyState != CopyDone || len(h.node.sent) != 1 || h.node.sent[0] != raw {
		t.Fatalf("copy: %+v sent %d", d, len(h.node.sent))
	}
}

func TestACopyNeedsTheInputsTheSignaturesCommitTo(t *testing.T) {
	// The explorer names other addresses (an attacker's, who has proofs
	// for them) as the inputs: their scripts don't verify
	h := newHarness(t)
	r := h.request(t, "req-1", dest, true)
	id, _, _ := donorTx(t, h.sha, r.Address, 99)
	attacker, script, _ := h.w.Keys.Address(5)
	for i := range h.sha.txs[id].Vin {
		h.sha.txs[id].Vin[i].Prevout.Address = attacker
		h.sha.txs[id].Vin[i].Prevout.ScriptPubKey = hex.EncodeToString(script)
	}
	prove(h, r, id, attacker)
	h.pass(t)
	d := h.deposits(t, r.ID, SHA256)[0]
	if d.CopyState != CopyNotPossible || !strings.Contains(d.CopyReason, "could not be verified") || len(h.node.sent) != 0 {
		t.Fatalf("%+v sent %d", d, len(h.node.sent))
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
	id, raw, addrs := donorTx(t, h.sha, r.Address, 99)
	prove(h, r, id, addrs...)
	h.node.refuse[raw] = "bad-txns-inputs-missingorspent"
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
	a := h.blake.sendProtected(t, r.Address, 0, 40_000, 1)
	h.node.put(t, h.w.Keys, 0, a, 0, 40_000, 9)
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
	// Not again at once; an hour later, alone, it goes through
	delete(h.node.refuse, "*")
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("tried again at once")
	}
	h.clock = h.clock.Add(time.Hour)
	h.pass(t)
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
	}
}

func TestDustIsKeptAndALostCoinIsGoneUntilItComesBack(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	dust := h.blake.sendProtected(t, r.Address, 0, 900, 1)
	coin := h.blake.sendProtected(t, r.Address, 0, 6000, 2)
	h.node.put(t, h.w.Keys, 0, dust, 0, 900, 8)
	h.node.put(t, h.w.Keys, 0, coin, 0, 6000, 1)
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("passed on")
	}
	// The unconfirmed one leaves the chain (replaced)
	delete(h.node.utxos, coin+":0")
	for i := 0; i < missingLimit; i++ {
		h.pass(t)
	}
	states := func() map[string]string {
		out := map[string]string{}
		for _, d := range h.deposits(t, r.ID, BLAKE2b) {
			out[d.Txid] = d.State
		}
		return out
	}
	if got := states(); got[dust] != Kept || got[coin] != Gone {
		t.Fatalf("%v", got)
	}
	// It is mined after all: it comes back, and is passed on
	h.node.put(t, h.w.Keys, 0, coin, 0, 6000, 6)
	h.pass(t)
	if got := states(); got[coin] != Ready && got[coin] != Forwarding {
		t.Fatalf("not back: %v", got)
	}
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
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

func TestAFailingExplorerDoesNotPauseTheService(t *testing.T) {
	h := newHarness(t)
	h.request(t, "req-1", "", false)
	h.sha.byAddress = nil
	h.sha.fail = errors.New("mempool.space down")
	h.clock = h.clock.Add(time.Minute)
	if err := h.w.Pass(context.Background()); err == nil {
		t.Fatal("the failure was not reported")
	}
	last, _ := h.store.Setting(context.Background(), SettingLastPass)
	if last != h.clock.UTC().Format(time.RFC3339) {
		t.Fatalf("last pass %q", last)
	}
}

const sweepTo = "bc1q0mgraq3342p0etdrmvnevurc4xdemtwhyl9q7j"

// lockedHarness: two SHA256 donations, locked together, the lock deep
func lockedHarness(t *testing.T) (*harness, Request, string) {
	h := newHarness(t)
	r := h.request(t, "req-1", "", false)
	h.sha.pay(txA, r.Address, 40_000, 98, "bc1qdonor")
	h.sha.pay(txB, r.Address, 30_000, 98, "bc1qdonor")
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatalf("locks %d", len(h.sha.broadcast))
	}
	lock := txid(h.sha.broadcast[0])
	h.sha.confirm(lock, 95)
	h.pass(t)
	if l, _ := h.store.Unswept(context.Background()); len(l) != 1 || l[0].ValueSat < 60_000 {
		t.Fatalf("unswept %+v", l)
	}
	h.w.SweepAddress = sweepTo
	return h, r, lock
}

func TestASweepSpendsTheLocks(t *testing.T) {
	h, r, lock := lockedHarness(t)
	h.pass(t)
	if len(h.sha.broadcast) != 2 {
		t.Fatalf("broadcast %d", len(h.sha.broadcast))
	}
	sweep := decode(t, h.sha.broadcast[1])
	if len(sweep.TxIn) != 1 || sweep.TxIn[0].PreviousOutPoint.String() != lock+":0" || len(sweep.TxOut) != 1 {
		t.Fatalf("sweep %+v", sweep)
	}
	sweeps, _ := h.store.SweepsIn(context.Background(), Sent)
	if len(sweeps) != 1 || sweeps[0].Dest != sweepTo {
		t.Fatalf("%+v", sweeps)
	}
	// Not again the same day
	h.pass(t)
	if len(h.sha.broadcast) != 2 {
		t.Fatal("swept twice")
	}
	h.sha.confirm(sweeps[0].Txid, 98)
	h.pass(t)
	if done, _ := h.store.SweepsIn(context.Background(), Done); len(done) != 1 {
		t.Fatalf("not done: %+v", done)
	}
	for _, d := range h.deposits(t, r.ID, SHA256) {
		if d.State != Locked {
			t.Fatalf("%s: %s", d.Txid[:4], d.State)
		}
	}
}

func TestNoSweepBelowTheMinimumOrWhenTheBLAKE2bNodeWouldTakeIt(t *testing.T) {
	h, _, _ := lockedHarness(t)
	h.w.SweepMinSat = 100_000
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatal("swept below the minimum")
	}
	h, _, _ = lockedHarness(t)
	h.node.allowAll = true
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatal("swept what the BLAKE2b node takes")
	}
}

func TestAFailedSweepReleasesItsLocks(t *testing.T) {
	h, _, _ := lockedHarness(t)
	h.sha.refuse = errors.New("explorers: HTTP 400")
	h.pass(t)
	if failed, _ := h.store.SweepsIn(context.Background(), FailedSend); len(failed) != 1 {
		t.Fatalf("%+v", failed)
	}
	if l, _ := h.store.Unswept(context.Background()); len(l) != 1 {
		t.Fatalf("not released: %+v", l)
	}
}

func TestNoSweepAddressNoSweep(t *testing.T) {
	h, _, _ := lockedHarness(t)
	h.w.SweepAddress = ""
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatal("swept without an address")
	}
}

func TestNoLockWhenTheBLAKE2bNodeWouldTakeIt(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.node.allowAll = true
	h.pass(t)
	if len(h.sha.broadcast) != 0 {
		t.Fatal("locked with a lock the BLAKE2b node takes")
	}
}

func TestAFailedLockWaitsAnHour(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.sha.refuse = errors.New("explorers: scriptpubkey")
	h.pass(t)
	if failed, _ := h.store.LocksIn(context.Background(), FailedSend); len(failed) != 1 {
		t.Fatalf("%+v", failed)
	}
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State != Confirmed {
		t.Fatalf("not released: %s", d.State)
	}
	h.sha.refuse = nil
	h.pass(t)
	if len(h.sha.broadcast) != 0 {
		t.Fatal("tried again within the hour")
	}
	h.clock = h.clock.Add(time.Hour)
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatalf("not tried again after the hour: %d", len(h.sha.broadcast))
	}
}

func TestNoLocksNearTheEndOfTheDataRules(t *testing.T) {
	h := newHarness(t)
	var logged bytes.Buffer
	h.w.Logger = log.New(&logged, "", 0)
	h.w.RDTSUntil = h.clock.Add(20 * 24 * time.Hour)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.pass(t)
	h.pass(t)
	if len(h.sha.broadcast) != 0 {
		t.Fatal("locked within a month of the end of the data rules")
	}
	if n := strings.Count(logged.String(), "ALERT: no locks"); n != 1 {
		t.Fatalf("alerted %d times", n)
	}
}

func TestASHA256DonationSpentElsewhereIsGone(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.sha.spent[txA+":1"] = strings.Repeat("4", 64)
	h.pass(t)
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State != Gone || len(h.sha.broadcast) != 0 {
		t.Fatalf("%+v", d)
	}
}

// lockedTwin: a SHA256 donation locked and 6 deep, its BLAKE2b twin deep
func lockedTwin(t *testing.T, h *harness, r Request) string {
	t.Helper()
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 6)
	h.pass(t)
	if len(h.sha.broadcast) != 1 {
		t.Fatalf("locks %d", len(h.sha.broadcast))
	}
	lock := txid(h.sha.broadcast[0])
	h.sha.confirm(lock, 95)
	return lock
}

func TestEveryExplorerMustSeeTheLock(t *testing.T) {
	h := newHarness(t)
	other := newExplorer() // a second SHA256 explorer that sees no lock
	h.w.Verifiers = []Explorer{h.sha, other}
	r := h.request(t, "req-1", dest, false)
	lockedTwin(t, h, r)
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("passed on while an explorer does not see the lock")
	}
	if b := h.deposits(t, r.ID, BLAKE2b)[0]; !strings.Contains(b.Hold, "explorer") {
		t.Fatalf("hold %q", b.Hold)
	}
	// The second explorer sees it too, as deep
	for k, v := range h.sha.spent {
		other.spent[k] = v
	}
	for k, v := range h.sha.txs {
		other.txs[k] = v
	}
	h.pass(t)
	if len(h.node.sent) != 1 {
		t.Fatalf("sent %d", len(h.node.sent))
	}
}

func TestALockOutOfTheChainHoldsTheForward(t *testing.T) {
	// Locked in the store (it was 6 deep), then reorganised out before the
	// twin is passed on: checked again at forward time, held
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.pass(t)
	lock := txid(h.sha.broadcast[0])
	h.sha.confirm(lock, 95)
	h.pass(t) // Locked; no BLAKE2b twin yet
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State != Locked {
		t.Fatalf("%s", d.State)
	}
	delete(h.sha.spent, txA+":1")
	h.sha.txs[lock].Status = esplora.Status{}
	h.node.put(t, h.w.Keys, 0, txA, 1, 50_000, 6)
	h.pass(t)
	h.pass(t)
	if len(h.node.sent) != 0 {
		t.Fatal("passed on with the lock out of the chain")
	}
}

func TestABroadcastThatWentOutIsNotFailed(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.sha.refuse = errors.New("explorers: timeout")
	// It went out anyway: an explorer has it
	h.sha.onRefuse = func(raw string) { h.sha.txs[txid(raw)] = &esplora.Tx{Txid: txid(raw)} }
	h.pass(t)
	if failed, _ := h.store.LocksIn(context.Background(), FailedSend); len(failed) != 0 {
		t.Fatalf("failed %+v", failed)
	}
	if sent, _ := h.store.LocksIn(context.Background(), Sent); len(sent) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State != Locking {
		t.Fatalf("%s", d.State)
	}
}

func TestADepositSpentByOurFailedLockComesBack(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.sha.refuse = errors.New("explorers: timeout")
	h.pass(t)
	failed, _ := h.store.LocksIn(context.Background(), FailedSend)
	if len(failed) != 1 {
		t.Fatalf("%+v", failed)
	}
	// It did go out, and an hour later the explorer shows the coin spent by it
	h.sha.refuse = nil
	h.sha.spent[txA+":1"] = failed[0].Txid
	h.clock = h.clock.Add(time.Hour)
	h.pass(t)
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State != Locking || d.LockID != failed[0].ID {
		t.Fatalf("not taken back: %+v", d)
	}
	if l, _ := h.store.LocksIn(context.Background(), Sent); len(l) != 1 || l[0].ID != failed[0].ID {
		t.Fatalf("lock %+v", l)
	}
}

func TestADonationOutOfTheListingIsNotGone(t *testing.T) {
	h := newHarness(t)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 0, "bc1qdonor") // unconfirmed
	h.pass(t)
	// Pushed out of the address listing; the transaction is still there
	h.sha.byAddress[r.Address] = nil
	for i := 0; i < missingLimit+1; i++ {
		h.pass(t)
	}
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State == Gone {
		t.Fatal("gone while its transaction exists")
	}
	// Confirmed meanwhile: counted
	h.sha.confirm(txA, 98)
	h.pass(t)
	if d := h.deposits(t, r.ID, SHA256)[0]; d.State == Seen || d.State == Gone {
		t.Fatalf("%s", d.State)
	}
}

func TestARequestTakesAtMostItsShareOfDeposits(t *testing.T) {
	h := newHarness(t)
	h.w.MaxDeposits = 3
	r := h.request(t, "req-1", dest, false)
	tx := esplora.Tx{Txid: txA, Status: esplora.Status{Confirmed: true, BlockHeight: 98}}
	for i := 0; i < 10; i++ {
		tx.Vout = append(tx.Vout, esplora.Vout{Address: r.Address, Value: 10_000})
	}
	h.sha.byAddress[r.Address] = []esplora.Tx{tx}
	h.sha.txs[txA] = &tx
	h.pass(t)
	if n := len(h.deposits(t, r.ID, SHA256)); n != 3 {
		t.Fatalf("deposits %d", n)
	}
}

func TestALockSpendsAtMostMaxTxInputs(t *testing.T) {
	h := newHarness(t)
	h.w.MaxTxInputs = 2
	r := h.request(t, "req-1", dest, false)
	tx := esplora.Tx{Txid: txA, Status: esplora.Status{Confirmed: true, BlockHeight: 98}}
	for i := 0; i < 5; i++ {
		tx.Vout = append(tx.Vout, esplora.Vout{Address: r.Address, Value: 10_000})
	}
	h.sha.byAddress[r.Address] = []esplora.Tx{tx}
	h.sha.txs[txA] = &tx
	h.pass(t)
	if len(h.sha.broadcast) != 1 || len(decode(t, h.sha.broadcast[0]).TxIn) != 2 {
		t.Fatalf("locks %d", len(h.sha.broadcast))
	}
}

func TestAFailingRequestDoesNotStarveTheOthers(t *testing.T) {
	h := newHarness(t)
	h.w.MaxPolls = 1
	bad := h.request(t, "req-bad", dest, false)
	good := h.request(t, "req-good", dest, false)
	h.sha.failFor = map[string]bool{bad.Address: true}
	h.sha.pay(txA, good.Address, 50_000, 98, "bc1qdonor")
	for i := 0; i < 3; i++ {
		h.clock = h.clock.Add(time.Minute)
		_ = h.w.Pass(context.Background())
	}
	if len(h.deposits(t, good.ID, SHA256)) != 1 {
		t.Fatal("the good request was never looked at")
	}
}

func TestLocksExposedAtTheEndOfTheDataRulesAreAlerted(t *testing.T) {
	h := newHarness(t)
	var logged bytes.Buffer
	h.w.Logger = log.New(&logged, "", 0)
	r := h.request(t, "req-1", dest, false)
	h.sha.pay(txA, r.Address, 50_000, 98, "bc1qdonor")
	h.pass(t)
	h.sha.confirm(txid(h.sha.broadcast[0]), 95)
	h.pass(t) // locked; its twin never appears on the BLAKE2b chain
	if strings.Contains(logged.String(), "no BLAKE2b twin") {
		t.Fatal("alerted long before the end")
	}
	h.w.RDTSUntil = h.clock.Add(60 * 24 * time.Hour)
	h.pass(t)
	h.pass(t)
	if n := strings.Count(logged.String(), "1 locked SHA256 donation(s) have no BLAKE2b twin"); n != 1 {
		t.Fatalf("alerted %d times: %s", n, logged.String())
	}
}
