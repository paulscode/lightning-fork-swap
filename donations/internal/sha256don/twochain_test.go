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

	// Our forward of the BLAKE2b twin, unified
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
	if err != nil || acc.Allowed || strings.Contains(acc.Reason, "missingorspent") {
		t.Fatalf("the SHA256 node on our forward: %+v %v", acc, err)
	}
	// The scam's step 4, before a lock: we hold the key, so we could sign
	// the same forward the ordinary way (the same txid), and the SHA256
	// chain would take it. This is what the lock removes.
	ordinary := resign(t, keys, signed.Raw, coin.ValueSat, script, txscript.SigHashDefault)
	if id, _ := txidOf(ordinary); id != signed.Txid {
		t.Fatal("re-signing changed the txid")
	}
	if acc, err := sha.TestAccept(ctx, ordinary); err != nil || !acc.Allowed {
		t.Fatalf("expected the SHA256 node to take the re-signed forward before a lock: %+v %v", acc, err)
	}

	// The lock, on the SHA256 chain: taken there; the BLAKE2b chain refuses
	// it for its data output while its coin is unspent there, and will not
	// mine it (consensus, not only relay)
	lockCoins := []Coin{{Txid: txid, Vout: uint32(vout), ValueSat: coin.ValueSat, KeyIndex: 0}}
	lock, err := SignLock(keys, lockCoins, 0, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := sha.TestAccept(ctx, lock.Raw); err != nil || !acc.Allowed {
		t.Fatalf("the SHA256 node refuses the lock: %+v %v", acc, err)
	}
	// Its mempool refuses it (for relay policy first); mining it is the
	// consensus question
	acc, err = b2b.TestAccept(ctx, lock.Raw)
	if err != nil || acc.Allowed {
		t.Fatalf("the BLAKE2b node takes the lock: %+v %v", acc, err)
	}
	var block any
	err = b2b.Call(ctx, "generateblock", []any{minerAddr, []string{lock.Raw}}, &block)
	if err == nil || !strings.Contains(err.Error(), "vout-script-toolarge") {
		t.Fatalf("the BLAKE2b node mined the lock: %v", err)
	}
	t.Logf("the BLAKE2b chain will not mine the lock: %v", err)
	if _, err := sha.Send(ctx, lock.Raw); err != nil {
		t.Fatal(err)
	}
	call(t, sha, "generatetoaddress", []any{3, minerAddr}, &mined)

	// Locked: our forward goes out on the BLAKE2b chain
	if _, err := b2b.Send(ctx, signed.Raw); err != nil {
		t.Fatal(err)
	}
	call(t, b2b, "generatetoaddress", []any{1, minerAddr}, &mined)
	if c, _ := b2b.TxOut(ctx, signed.Txid, 0, false); c == nil || c.ValueSat != signed.AmountSat {
		t.Fatalf("the forward did not pay the return address: %+v", c)
	}

	// The scam's step 4, after the lock: the re-signed forward is refused
	// by the SHA256 chain, its coin already spent there by the lock. Its
	// output exists on the BLAKE2b chain only, whoever signs what.
	acc, err = sha.TestAccept(ctx, ordinary)
	if err != nil || acc.Allowed || !strings.Contains(acc.Reason, "missing") {
		t.Fatalf("the SHA256 node on the re-signed forward after the lock: %+v %v", acc, err)
	}
	t.Logf("after the lock the SHA256 chain refuses even a re-signed forward: %s", acc.Reason)

	// The sweep of the lock's output: the SHA256 chain only
	var sweepTo string
	call(t, miner, "getnewaddress", nil, &sweepTo)
	sweep, err := SignSHA256(keys, []Coin{{Txid: lock.Txid, Vout: 0, ValueSat: lock.AmountSat,
		KeyIndex: 0, Branch: Locks}}, sweepTo, 2, params)
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
