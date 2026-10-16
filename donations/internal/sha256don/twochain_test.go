package sha256don

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/paulscode/lightning-fork-swap/donations/internal/knots"
)

// TestTwoChains runs against two regtest nodes that share their first
// blocks, set up by deploy/regtest/sha256-donations-twochain.sh: a BLAKE2b
// Knots (TWOCHAIN_BLAKE2B) and a SHA256 one (TWOCHAIN_SHA256), with a
// "donor" wallet on the SHA256 node holding only coins mined before the
// fork, which exist on both chains, and a "miner" wallet.
//
// A donation from those coins, copied to the BLAKE2b chain as the network
// would, gives us the same coin on both chains. Our forward of it must be
// accepted by the BLAKE2b node and refused by the SHA256 node, where the
// coin is just as unspent; the same forward signed the ordinary way must be
// accepted by both, which shows the test can tell them apart.
func TestTwoChains(t *testing.T) {
	b2bURL, shaURL := os.Getenv("TWOCHAIN_BLAKE2B"), os.Getenv("TWOCHAIN_SHA256")
	if b2bURL == "" || shaURL == "" {
		t.Skip("TWOCHAIN_BLAKE2B and TWOCHAIN_SHA256 not set (deploy/regtest/sha256-donations-twochain.sh)")
	}
	ctx := context.Background()
	b2b, sha := client(t, b2bURL, ""), client(t, shaURL, "")
	donor, miner := client(t, shaURL, "donor"), client(t, shaURL, "miner")
	params := &chaincfg.RegressionNetParams
	seed, _ := hex.DecodeString(vectorSeed)
	keys, err := NewKeys(seed, params)
	if err != nil {
		t.Fatal(err)
	}
	ours, script, err := keys.Address(0)
	if err != nil {
		t.Fatal(err)
	}

	// The donation, on the SHA256 chain
	var txid string
	call(t, donor, "sendtoaddress", []any{ours, 1.0}, &txid)
	var donation struct {
		Hex string `json:"hex"`
	}
	call(t, donor, "gettransaction", []any{txid}, &donation)
	tx := decodeHex(t, donation.Hex)
	for _, in := range tx.TxIn {
		var prev struct {
			Blockheight int64 `json:"blockheight"`
		}
		call(t, donor, "gettransaction", []any{in.PreviousOutPoint.Hash.String()}, &prev)
		if prev.Blockheight >= 20 {
			t.Fatalf("the donor spent a coin from after the fork (height %d)", prev.Blockheight)
		}
	}
	vout := -1
	for i, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, script) {
			vout = i
		}
	}
	if vout < 0 {
		t.Fatal("the donation does not pay our address")
	}
	var mined []string
	var minerAddr string
	call(t, miner, "getnewaddress", nil, &minerAddr)
	call(t, sha, "generatetoaddress", []any{1, minerAddr}, &mined)

	// The network copies it to the BLAKE2b chain
	if _, err := b2b.Send(ctx, donation.Hex); err != nil {
		t.Fatalf("the BLAKE2b node refused the copy: %v", err)
	}
	call(t, b2b, "generatetoaddress", []any{6, minerAddr}, &mined)
	coin, err := b2b.TxOut(ctx, txid, uint32(vout), false)
	if err != nil || coin == nil || coin.Confirmations < 6 {
		t.Fatalf("our coin on the BLAKE2b chain: %+v %v", coin, err)
	}
	if c, _ := sha.TxOut(ctx, txid, uint32(vout), false); c == nil {
		t.Fatal("the donation is not on the SHA256 chain")
	}

	// Our forward: accepted by the BLAKE2b node, refused by the SHA256 one
	var ret string
	call(t, miner, "getnewaddress", nil, &ret)
	coins := []Coin{{Txid: txid, Vout: uint32(vout), ValueSat: coin.ValueSat, KeyIndex: 0}}
	signed, err := Sign(keys, coins, ret, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := b2b.TestAccept(ctx, signed.Raw); err != nil || !acc.Allowed {
		t.Fatalf("the BLAKE2b node refuses our forward: %+v %v", acc, err)
	}
	acc, err := sha.TestAccept(ctx, signed.Raw)
	if err != nil || acc.Allowed {
		t.Fatalf("the SHA256 node takes our forward: %+v %v", acc, err)
	}
	if strings.Contains(acc.Reason, "missingorspent") {
		t.Fatalf("refused for its coin, not its signature: %s", acc.Reason)
	}
	t.Logf("the SHA256 node refuses the forward: %s", acc.Reason)

	// A sweep of the donation on the SHA256 chain, while its twin is
	// unspent on the BLAKE2b chain: valid on both, the danger an anchor
	// removes
	var sweepTo string
	call(t, miner, "getnewaddress", nil, &sweepTo)
	early, err := SignSHA256(keys, coins, sweepTo, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := sha.TestAccept(ctx, early.Raw); err != nil || !acc.Allowed {
		t.Fatalf("the SHA256 node refuses a sweep: %+v %v", acc, err)
	}
	if acc, err := b2b.TestAccept(ctx, early.Raw); err != nil || !acc.Allowed {
		t.Fatalf("expected the BLAKE2b node to take a sweep of an unforwarded twin: %+v %v", acc, err)
	}

	// The control: the same forward signed the ordinary way
	ordinary := resign(t, keys, signed.Raw, coin.ValueSat, script, txscript.SigHashDefault)
	if acc, err := sha.TestAccept(ctx, ordinary); err != nil || !acc.Allowed {
		t.Fatalf("control: the SHA256 node refuses an ordinary signature: %+v %v", acc, err)
	}
	if acc, err := b2b.TestAccept(ctx, ordinary); err != nil || !acc.Allowed {
		t.Fatalf("control: the BLAKE2b node refuses an ordinary signature: %+v %v", acc, err)
	}

	// Passed on, on the BLAKE2b chain; the donation stays on the SHA256 chain
	if _, err := b2b.Send(ctx, signed.Raw); err != nil {
		t.Fatal(err)
	}
	call(t, b2b, "generatetoaddress", []any{1, minerAddr}, &mined)
	if c, _ := b2b.TxOut(ctx, txid, uint32(vout), true); c != nil {
		t.Fatal("our coin is unspent on the BLAKE2b chain after the forward")
	}
	if _, err := sha.Send(ctx, signed.Raw); err == nil {
		t.Fatal("the SHA256 node relayed the forward")
	}
	if c, _ := sha.TxOut(ctx, txid, uint32(vout), true); c == nil {
		t.Fatal("the donation was spent on the SHA256 chain")
	}

	// Now the twin is passed on: the sweep is an anchor's, valid on the
	// SHA256 chain only, refused by the BLAKE2b node for a missing coin
	sweep, err := SignSHA256(keys, coins, sweepTo, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	acc, err = b2b.TestAccept(ctx, sweep.Raw)
	if err != nil || acc.Allowed || !strings.Contains(acc.Reason, "missing") {
		t.Fatalf("the BLAKE2b node on the sweep: %+v %v", acc, err)
	}
	if _, err := sha.Send(ctx, sweep.Raw); err != nil {
		t.Fatalf("the SHA256 node refuses the sweep: %v", err)
	}
	call(t, sha, "generatetoaddress", []any{1, minerAddr}, &mined)
	if c, _ := sha.TxOut(ctx, sweep.Txid, 0, false); c == nil || c.ValueSat != sweep.AmountSat {
		t.Fatalf("the sweep did not pay the operator: %+v", c)
	}
	t.Logf("swept on the SHA256 chain; the BLAKE2b node refuses it: %s", acc.Reason)
}

func client(t *testing.T, raw, wallet string) *knots.Client {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	user := u.User.Username()
	u.User = nil
	if wallet != "" {
		u.Path = "/wallet/" + wallet
	}
	return knots.New(u.String(), user, pw)
}

func call(t *testing.T, c *knots.Client, method string, params []any, out any) {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	if err := c.Call(context.Background(), method, params, out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func decodeHex(t *testing.T, raw string) *wire.MsgTx {
	t.Helper()
	b, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	return tx
}

// resign signs a one-input forward again with another hash type
func resign(t *testing.T, keys *Keys, raw string, value int64, script []byte, hashType txscript.SigHashType) string {
	t.Helper()
	tx := decodeHex(t, raw)
	f := txscript.NewMultiPrevOutFetcher(nil)
	f.AddPrevOut(tx.TxIn[0].PreviousOutPoint, wire.NewTxOut(value, script))
	priv, _ := keys.Private(0)
	w, err := txscript.TaprootWitnessSignature(tx, txscript.NewTxSigHashes(tx, f), 0,
		value, script, hashType, priv)
	if err != nil {
		t.Fatal(err)
	}
	tx.TxIn[0].Witness = w
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	return hex.EncodeToString(buf.Bytes())
}
