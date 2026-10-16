package replay

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
)

// chain is a fake explorer of one chain.
type chain struct {
	txs       map[string]*esplora.Tx
	hex       map[string]string
	statuses  map[string]*esplora.Status
	outspends map[string]*esplora.Outspend // "txid:vout"
	tip       int64
	fail      error
	calls     int
}

func newChain() *chain {
	return &chain{txs: map[string]*esplora.Tx{}, hex: map[string]string{},
		statuses: map[string]*esplora.Status{}, outspends: map[string]*esplora.Outspend{}, tip: 980_000}
}

func (c *chain) Tx(_ context.Context, txid string) (*esplora.Tx, error) {
	c.calls++
	if c.fail != nil {
		return nil, c.fail
	}
	if tx, ok := c.txs[txid]; ok {
		return tx, nil
	}
	return nil, esplora.ErrNotFound
}

func (c *chain) TxHex(_ context.Context, txid string) (string, error) {
	c.calls++
	if c.fail != nil {
		return "", c.fail
	}
	if h, ok := c.hex[txid]; ok {
		return h, nil
	}
	return "", esplora.ErrNotFound
}

func (c *chain) TxStatus(_ context.Context, txid string) (*esplora.Status, error) {
	c.calls++
	if c.fail != nil {
		return nil, c.fail
	}
	if s, ok := c.statuses[txid]; ok {
		return s, nil
	}
	if tx, ok := c.txs[txid]; ok {
		return &tx.Status, nil
	}
	// As explorers do: unknown is "unconfirmed"
	return &esplora.Status{}, nil
}

func (c *chain) Outspend(_ context.Context, txid string, vout uint32) (*esplora.Outspend, error) {
	c.calls++
	if c.fail != nil {
		return nil, c.fail
	}
	if o, ok := c.outspends[fmt.Sprintf("%s:%d", txid, vout)]; ok {
		return o, nil
	}
	return &esplora.Outspend{}, nil
}

func (c *chain) TipHeight(context.Context) (int64, error) { return c.tip, c.fail }

// key n, and its taproot output script
func key(n byte) (*btcec.PrivateKey, []byte) {
	priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{n}, 32))
	script, _ := txscript.PayToTaprootScript(txscript.ComputeTaprootKeyNoScript(priv.PubKey()))
	return priv, script
}

// spend is an input: the coin (txid:vout), its value, and its key
type spend struct {
	txid  string
	vout  uint32
	value int64
	key   byte
}

// build makes a real transaction on a chain: inputs spending taproot coins
// of keys, signed with hashType, one output to key 99; it is put in the
// chain at height (0: in the mempool) and returned with its txid.
func build(c *chain, height int64, hashType txscript.SigHashType, ins ...spend) string {
	msg := wire.NewMsgTx(2)
	f := txscript.NewMultiPrevOutFetcher(nil)
	var total int64
	for _, in := range ins {
		h, _ := chainhash.NewHashFromStr(in.txid)
		op := wire.NewOutPoint(h, in.vout)
		msg.AddTxIn(wire.NewTxIn(op, nil, nil))
		_, script := key(in.key)
		f.AddPrevOut(*op, wire.NewTxOut(in.value, script))
		total += in.value
	}
	_, out := key(99)
	msg.AddTxOut(wire.NewTxOut(total-500, out))
	sh := txscript.NewTxSigHashes(msg, f)
	tx := &esplora.Tx{Status: esplora.Status{Confirmed: height > 0, BlockHeight: height},
		Vout: []esplora.Vout{{ScriptPubKey: hex.EncodeToString(out), Value: total - 500}}}
	for i, in := range ins {
		priv, script := key(in.key)
		w, err := txscript.TaprootWitnessSignature(msg, sh, i, in.value, script, hashType, priv)
		if err != nil {
			panic(err)
		}
		msg.TxIn[i].Witness = w
		tx.Vin = append(tx.Vin, esplora.Vin{Txid: in.txid, Vout: in.vout,
			Witness: []string{hex.EncodeToString(w[0])},
			Prevout: &esplora.Prevout{ScriptPubKey: hex.EncodeToString(script), Type: "v1_p2tr",
				Address: fmt.Sprintf("bc1pkey%d", in.key), Value: in.value}})
	}
	var buf bytes.Buffer
	_ = msg.Serialize(&buf)
	tx.Txid = msg.TxHash().String()
	c.txs[tx.Txid] = tx
	c.hex[tx.Txid] = hex.EncodeToString(buf.Bytes())
	return tx.Txid
}

// coin makes a confirmed transaction at height paying key k (its inputs
// are a made-up coinbase-free coin it never shows)
func coin(c *chain, height int64, k byte) string {
	tx := &esplora.Tx{Txid: fmt.Sprintf("%064x", int64(height)*1000+int64(k)),
		Status: esplora.Status{Confirmed: true, BlockHeight: height}}
	c.txs[tx.Txid] = tx
	return tx.Txid
}

const (
	unified  = txscript.SigHashAll | txscript.SigHashUnified
	ordinary = txscript.SigHashDefault
	preFork  = ForkHeight - 1
)

func judge(t *testing.T, txid string, b, s *chain) Result {
	t.Helper()
	got, err := Check(context.Background(), txid, b, s)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSignaturesAreJudgedByRunningTheScripts(t *testing.T) {
	b, s := newChain(), newChain()
	parent := coin(b, preFork, 1)
	s.txs[parent] = b.txs[parent]

	// Signed the ordinary way, from a coin both chains have: at risk
	d := build(b, 979_000, ordinary, spend{parent, 0, 10_000, 1})
	if got := judge(t, d, b, s); got.Verdict != AtRisk || got.Inputs[0].Sha256 != "unspent" ||
		!got.Inputs[0].PreFork || len(got.AtRiskAddresses) != 1 {
		t.Fatalf("ordinary: %+v", got)
	}
	// Signed unified: the SHA256 chain refuses it as it is
	d = build(b, 979_000, unified, spend{parent, 0, 10_000, 1})
	if got := judge(t, d, b, s); got.Verdict != Protected || got.Inputs[0].Protection != "protected" {
		t.Fatalf("unified: %+v", got)
	}
}

func TestADecoyHashTypeProtectsNothing(t *testing.T) {
	// A P2WSH input whose script drops an item shaped like a unified
	// signature and checks an ordinary one: valid on both chains
	b, s := newChain(), newChain()
	priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{7}, 32))
	witnessScript, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_DROP).
		AddData(priv.PubKey().SerializeCompressed()).AddOp(txscript.OP_CHECKSIG).Script()
	h := chainhash.HashH(witnessScript)
	pkScript, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(h[:]).Script()
	parent := coin(b, preFork, 7)
	s.txs[parent] = b.txs[parent]

	msg := wire.NewMsgTx(2)
	ph, _ := chainhash.NewHashFromStr(parent)
	op := wire.NewOutPoint(ph, 0)
	msg.AddTxIn(wire.NewTxIn(op, nil, nil))
	_, out := key(99)
	msg.AddTxOut(wire.NewTxOut(9_000, out))
	f := txscript.NewMultiPrevOutFetcher(nil)
	f.AddPrevOut(*op, wire.NewTxOut(10_000, pkScript))
	sig, err := txscript.RawTxInWitnessSignature(msg, txscript.NewTxSigHashes(msg, f), 0, 10_000,
		witnessScript, txscript.SigHashAll, priv)
	if err != nil {
		t.Fatal(err)
	}
	decoy := append(append([]byte{}, sig[:len(sig)-1]...), 0x21)
	msg.TxIn[0].Witness = wire.TxWitness{sig, decoy, witnessScript}
	var buf bytes.Buffer
	_ = msg.Serialize(&buf)
	d := msg.TxHash().String()
	b.txs[d] = &esplora.Tx{Txid: d, Status: esplora.Status{Confirmed: true, BlockHeight: 979_000},
		Vin: []esplora.Vin{{Txid: parent, Vout: 0, Witness: []string{
			hex.EncodeToString(sig), hex.EncodeToString(decoy), hex.EncodeToString(witnessScript)},
			Prevout: &esplora.Prevout{ScriptPubKey: hex.EncodeToString(pkScript), Type: "v0_p2wsh",
				Address: "bc1qdecoy", Value: 10_000}}}}
	b.hex[d] = hex.EncodeToString(buf.Bytes())

	got := judge(t, d, b, s)
	if got.Verdict != AtRisk || got.Inputs[0].Protection != "unprotected" {
		t.Fatalf("a decoy counted: %+v", got)
	}
	// The reader still reports the hash types it saw
	if !strings.Contains(strings.Join(got.Inputs[0].HashTypes, ","), "0x21") {
		t.Fatalf("hash types %v", got.Inputs[0].HashTypes)
	}
}

func TestSpendsElsewhereCountOnlyWhenDeep(t *testing.T) {
	b, s := newChain(), newChain()
	parent := coin(b, preFork, 1)
	s.txs[parent] = b.txs[parent]
	d := build(b, 979_000, ordinary, spend{parent, 0, 10_000, 1})
	other := strings.Repeat("e", 64)
	at := func(st *esplora.Status) {
		s.outspends[parent+":0"] = &esplora.Outspend{Spent: true, Txid: other, Status: st}
	}

	at(nil) // no status given
	if got := judge(t, d, b, s); got.Verdict != AtRisk {
		t.Fatalf("no status: %v", got.Verdict)
	}
	at(&esplora.Status{}) // in a mempool: may be replaced
	if got := judge(t, d, b, s); got.Verdict != AtRisk || !strings.Contains(got.Inputs[0].Sha256, "not yet deep") {
		t.Fatalf("unconfirmed: %+v", got.Inputs[0])
	}
	at(&esplora.Status{Confirmed: true, BlockHeight: s.tip - SpendDepth + 2}) // 5 deep
	if got := judge(t, d, b, s); got.Verdict != AtRisk {
		t.Fatalf("5 deep: %v", got.Verdict)
	}
	at(&esplora.Status{Confirmed: true, BlockHeight: s.tip - SpendDepth + 1}) // 6 deep
	if got := judge(t, d, b, s); got.Verdict != TwinMoved {
		t.Fatalf("6 deep: %v", got.Verdict)
	}
	// Copied there and confirmed
	s.outspends[parent+":0"] = &esplora.Outspend{Spent: true, Txid: d}
	s.statuses[d] = &esplora.Status{Confirmed: true, BlockHeight: 979_500}
	if got := judge(t, d, b, s); got.Verdict != Replayed || got.ReplayedHeight != 979_500 {
		t.Fatalf("copied: %+v", got)
	}
}

func TestMissingOrWrongDataIsNeverProtection(t *testing.T) {
	b, s := newChain(), newChain()
	parent := coin(b, preFork, 1)
	s.txs[parent] = b.txs[parent]
	d := build(b, 979_000, unified, spend{parent, 0, 10_000, 1})

	// A spent output left out (not a coinbase): unknown, not "new coins"
	saved := b.txs[d].Vin[0].Prevout
	b.txs[d].Vin[0].Prevout = nil
	if got := judge(t, d, b, s); got.Verdict != Unknown {
		t.Fatalf("missing prevout: %v", got.Verdict)
	}
	b.txs[d].Vin[0].Prevout = saved

	// Bytes that are not the transaction asked about
	other := build(b, 979_000, unified, spend{parent, 0, 9_000, 1})
	right := b.hex[d]
	b.hex[d] = b.hex[other]
	if got := judge(t, d, b, s); got.Verdict != Unknown {
		t.Fatalf("other bytes: %v", got.Verdict)
	}
	b.hex[d] = right

	// A spent output's value that the signature does not commit to
	b.txs[d].Vin[0].Prevout.Value = 20_000
	if got := judge(t, d, b, s); got.Verdict != Unknown {
		t.Fatalf("wrong amount: %v", got.Verdict)
	}
	b.txs[d].Vin[0].Prevout.Value = 10_000

	// The SHA256-chain explorers down
	s.fail = errors.New("down")
	if got := judge(t, d, b, s); got.Verdict != Protected {
		t.Fatalf("protection needs only this chain's data: %v", got.Verdict)
	}
	o := build(b, 979_000, ordinary, spend{parent, 0, 10_000, 1})
	if got := judge(t, o, b, s); got.Verdict != Unknown {
		t.Fatalf("explorers down: %v", got.Verdict)
	}
}

func TestCheckFollowsTheAncestry(t *testing.T) {
	// A chain of ordinary transactions after the fork, from a coin made
	// before it: copyable, so the donation is at risk
	b, s := newChain(), newChain()
	start := coin(b, preFork, 1)
	s.txs[start] = b.txs[start]
	chain := func(n int, signLast txscript.SigHashType) (string, []string) {
		prev, ids := start, []string{}
		for i := 0; i < n; i++ {
			ht := ordinary
			if i == n-1 {
				ht = signLast
			}
			k := byte(1)
			if i > 0 {
				k = 99
			}
			prev = build(b, ForkHeight+int64(i), ht, spend{prev, 0, int64(100_000 - i*500), k})
			ids = append(ids, prev)
		}
		d := build(b, 979_000, ordinary, spend{prev, 0, int64(100_000 - n*500), 99})
		return d, ids
	}
	d, _ := chain(3, ordinary)
	got := judge(t, d, b, s)
	if got.Verdict != AtRisk || got.Inputs[0].Sha256 != "copyable" {
		t.Fatalf("copyable: %+v", got)
	}

	// An earlier transaction signed unified: it can never get there
	d, _ = chain(3, unified)
	if got := judge(t, d, b, s); got.Verdict != Protected || !strings.Contains(got.Reason, "refuses it") {
		t.Fatalf("unified ancestor: %v (%s)", got.Verdict, got.Reason)
	}

	// The coin before the fork spent there by another transaction, deep
	d, _ = chain(3, ordinary)
	s.outspends[start+":0"] = &esplora.Outspend{Spent: true, Txid: strings.Repeat("e", 64),
		Status: &esplora.Status{Confirmed: true, BlockHeight: s.tip - 10}}
	if got := judge(t, d, b, s); got.Verdict != Protected {
		t.Fatalf("moved early coin: %v (%s)", got.Verdict, got.Reason)
	}
	// ... not deep yet: still at risk
	s.outspends[start+":0"].Status = &esplora.Status{}
	if got := judge(t, d, b, s); got.Verdict != AtRisk {
		t.Fatalf("moved early coin, unconfirmed: %v", got.Verdict)
	}
	delete(s.outspends, start+":0")

	// Mined after the fork: never there
	cb := &esplora.Tx{Txid: strings.Repeat("c", 64), Vin: []esplora.Vin{{IsCoinbase: true}},
		Status: esplora.Status{Confirmed: true, BlockHeight: ForkHeight + 5}}
	b.txs[cb.Txid] = cb
	b.hex[cb.Txid] = "00"
	d = build(b, 979_000, ordinary, spend{cb.Txid, 0, 50_000, 1})
	if got := judge(t, d, b, s); got.Verdict != Protected || !strings.Contains(got.Reason, "mined after the fork") {
		t.Fatalf("coinbase ancestor: %v (%s)", got.Verdict, got.Reason)
	}

	// Longer than is followed: unknown, never protected
	d, _ = chain(MaxAncestors+3, ordinary)
	if got := judge(t, d, b, s); got.Verdict != Unknown || !strings.Contains(got.Reason, "by hand") {
		t.Fatalf("long: %v (%s)", got.Verdict, got.Reason)
	}
}

func TestCheckSHA256NamesTheChainsTheOtherWay(t *testing.T) {
	// A SHA256-chain transaction from coins mined after the fork there
	s, b := newChain(), newChain()
	cb := &esplora.Tx{Txid: strings.Repeat("c", 64), Vin: []esplora.Vin{{IsCoinbase: true}},
		Status: esplora.Status{Confirmed: true, BlockHeight: ForkHeight + 5}}
	s.txs[cb.Txid] = cb
	d := build(s, 979_000, ordinary, spend{cb.Txid, 0, 50_000, 1})
	got, err := CheckSHA256(context.Background(), d, s, b)
	if err != nil || got.Verdict != Protected {
		t.Fatalf("%v %v", got.Verdict, err)
	}
	if !strings.Contains(got.Reason, "never exist on the BLAKE2b chain") {
		t.Fatalf("reason: %s", got.Reason)
	}
	// A SHA256-chain transaction signed the ordinary way is valid on the
	// BLAKE2b chain too, as signed: not protected by its signatures
	parent := coin(s, preFork, 1)
	b.txs[parent] = s.txs[parent]
	d = build(s, 979_000, ordinary, spend{parent, 0, 10_000, 1})
	if got, _ := CheckSHA256(context.Background(), d, s, b); got.Verdict != AtRisk {
		t.Fatalf("ordinary SHA256 tx: %v (%s)", got.Verdict, got.Reason)
	}
}

func TestCheckNeedsTheDonation(t *testing.T) {
	if _, err := Check(context.Background(), strings.Repeat("d", 64), newChain(), newChain()); err == nil {
		t.Fatal("a missing donation was judged")
	}
}

func TestTooManyInputsAreLeftForAPerson(t *testing.T) {
	b, s := newChain(), newChain()
	tx := &esplora.Tx{Txid: strings.Repeat("d", 64)}
	for i := 0; i <= MaxInputs; i++ {
		tx.Vin = append(tx.Vin, esplora.Vin{Txid: strings.Repeat("a", 64), Vout: uint32(i)})
	}
	b.txs[tx.Txid] = tx
	got := judge(t, tx.Txid, b, s)
	if got.Verdict != Unknown || !strings.Contains(got.Reason, "by hand") || s.calls != 0 {
		t.Fatalf("%v (%s), %d calls", got.Verdict, got.Reason, s.calls)
	}
}

func TestNextCheck(t *testing.T) {
	seen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(days int) time.Time { return seen.Add(time.Duration(days) * 24 * time.Hour) }
	cases := []struct {
		v    Verdict
		days int
		want time.Duration // 0 = never
	}{
		{Pending, 0, time.Minute},
		{AtRisk, 1, time.Hour},
		{AtRisk, 31, 24 * time.Hour},
		{AtRisk, 366, 0},
		{Unknown, 6, time.Hour},
		{Unknown, 8, 24 * time.Hour},
		{Unknown, 31, 0},
		{Protected, 0, 0},
		{TwinMoved, 0, 0},
		{Replayed, 0, 0},
	}
	for _, c := range cases {
		got := NextCheck(c.v, seen, at(c.days))
		if c.want == 0 && !got.IsZero() || c.want != 0 && got.Sub(at(c.days)) != c.want {
			t.Errorf("%s after %d days: %v", c.v, c.days, got)
		}
		if c.v.Final() != (c.v == Protected || c.v == TwinMoved || c.v == Replayed) {
			t.Errorf("%s final", c.v)
		}
	}
}
