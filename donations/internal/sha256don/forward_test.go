package sha256don

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/paulscode/lightning-fork-swap/donations/internal/sighash"
)

const dest = "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"

func coins() []Coin {
	return []Coin{
		{Txid: strings.Repeat("a", 64), Vout: 1, ValueSat: 60_000, KeyIndex: 0},
		{Txid: strings.Repeat("b", 64), Vout: 0, ValueSat: 40_000, KeyIndex: 1},
	}
}

func decode(t *testing.T, raw string) *wire.MsgTx {
	t.Helper()
	b, _ := hex.DecodeString(raw)
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	return tx
}

func fetcherFor(t *testing.T, k *Keys, cs []Coin) txscript.PrevOutputFetcher {
	f := txscript.NewMultiPrevOutFetcher(nil)
	for _, c := range cs {
		h, _ := chainhash.NewHashFromStr(c.Txid)
		_, script, err := k.Address(c.KeyIndex)
		if err != nil {
			t.Fatal(err)
		}
		f.AddPrevOut(*wire.NewOutPoint(h, c.Vout), wire.NewTxOut(c.ValueSat, script))
	}
	return f
}

func TestSignsUnified(t *testing.T) {
	k := vectorKeys(t)
	s, err := Sign(k, coins(), dest, 2, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	if s.AmountSat+s.FeeSat != 100_000 || s.FeeSat != 2*s.VSize {
		t.Fatalf("amount %d fee %d vsize %d", s.AmountSat, s.FeeSat, s.VSize)
	}
	tx := decode(t, s.Raw)
	if tx.TxHash().String() != s.Txid || len(tx.TxIn) != 2 || len(tx.TxOut) != 1 {
		t.Fatalf("%+v", tx)
	}
	if err := CheckUnified(tx, fetcherFor(t, k, coins())); err != nil {
		t.Fatal(err)
	}
	// The replay checker reads every input as protected
	for i, in := range tx.TxIn {
		read, err := sighash.Read(sighash.Input{ScriptType: "v1_p2tr",
			Witness: []string{hex.EncodeToString(in.Witness[0])}})
		if err != nil || read.Protection != sighash.Protected {
			t.Fatalf("input %d: %+v %v", i, read, err)
		}
	}
}

func TestRefusesAnInputTheSHA256ChainWouldTake(t *testing.T) {
	k := vectorKeys(t)
	s, err := Sign(k, coins(), dest, 2, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, s.Raw)
	f := fetcherFor(t, k, coins())
	// Re-sign the second input the way a SHA256-chain wallet would
	priv, _ := k.Private(1)
	_, script, _ := k.Address(1)
	w, err := txscript.TaprootWitnessSignature(tx, txscript.NewTxSigHashes(tx, f), 1,
		40_000, script, txscript.SigHashAll, priv)
	if err != nil {
		t.Fatal(err)
	}
	tx.TxIn[1].Witness = w
	if err := CheckUnified(tx, f); err == nil {
		t.Fatal("an input without the opt-in passed")
	}
	// A unified byte on a signature that doesn't match: refused too
	tx.TxIn[1].Witness = wire.TxWitness{append(make([]byte, 64), byte(HashType))}
	if err := CheckUnified(tx, f); err == nil {
		t.Fatal("a bad signature passed")
	}
}

func TestSignRefuses(t *testing.T) {
	k := vectorKeys(t)
	small := []Coin{{Txid: strings.Repeat("c", 64), ValueSat: 1100}}
	if _, err := Sign(k, small, dest, 5, &chaincfg.MainNetParams); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("dust: %v", err)
	}
	if _, err := Sign(k, coins(), "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", 2, &chaincfg.MainNetParams); err == nil {
		t.Fatal("a testnet address was taken")
	}
	if _, err := Sign(k, nil, dest, 2, &chaincfg.MainNetParams); err == nil {
		t.Fatal("no coins was taken")
	}
	// Below 1 sat/vB is raised to 1
	s, err := Sign(k, coins(), dest, 0.1, &chaincfg.MainNetParams)
	if err != nil || s.FeeSat != s.VSize {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestSignsForTheSHA256Chain(t *testing.T) {
	k := vectorKeys(t)
	s, err := SignSHA256(k, coins(), dest, 3, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, s.Raw)
	f := fetcherFor(t, k, coins())
	if err := CheckSHA256(tx, f); err != nil {
		t.Fatal(err)
	}
	if s.FeeSat != 3*s.VSize || s.AmountSat+s.FeeSat != 100_000 {
		t.Fatalf("%+v", s)
	}
	// Not what a forward may be, and the reverse
	if err := CheckUnified(tx, f); err == nil {
		t.Fatal("an ordinary signature passed as unified")
	}
	u, _ := Sign(k, coins(), dest, 3, &chaincfg.MainNetParams)
	if err := CheckSHA256(decode(t, u.Raw), f); err == nil {
		t.Fatal("a unified signature passed on the SHA256 chain")
	}
}

func TestALockIsSHA256OnlyByConsensus(t *testing.T) {
	k := vectorKeys(t)
	s, err := SignLock(k, coins(), 7, 3, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, s.Raw)
	if len(tx.TxOut) != 2 {
		t.Fatalf("outputs %d", len(tx.TxOut))
	}
	_, lockScript, _ := k.AddressAt(Locks, 7)
	if !bytes.Equal(tx.TxOut[0].PkScript, lockScript) || tx.TxOut[0].Value != s.AmountSat {
		t.Fatal("the lock does not pay the hot wallet's lock address")
	}
	data := tx.TxOut[1]
	if data.Value != 0 || data.PkScript[0] != txscript.OP_RETURN || len(data.PkScript) != 103 ||
		!bytes.Contains(data.PkScript, []byte("invalid on the BLAKE2b chain by design")) {
		t.Fatalf("data output %x", data.PkScript)
	}
	if !BreaksRDTS(tx) {
		t.Fatal("the lock does not break the BLAKE2b chain's data rules")
	}
	if err := CheckSHA256(tx, fetcherFor(t, k, coins())); err != nil {
		t.Fatal(err)
	}
	// A forward or a sweep does not
	f, _ := Sign(k, coins(), dest, 3, &chaincfg.MainNetParams)
	if BreaksRDTS(decode(t, f.Raw)) {
		t.Fatal("a forward breaks the data rules")
	}
	// An 83-byte OP_RETURN is within them; 84 is not
	small := wire.NewMsgTx(2)
	ok, _ := txscript.NullDataScript(make([]byte, 80))
	small.AddTxOut(wire.NewTxOut(0, ok))
	if len(ok) != 83 || BreaksRDTS(small) {
		t.Fatalf("an 83-byte OP_RETURN (%d)", len(ok))
	}
	big, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_RETURN).AddData(make([]byte, 81)).Script()
	small.TxOut[0].PkScript = big
	if !BreaksRDTS(small) {
		t.Fatal("an 84-byte OP_RETURN")
	}
}
