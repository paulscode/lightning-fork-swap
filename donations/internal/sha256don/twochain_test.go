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
	lock, err := SignLock(keys, lockCoins, 0, nil, 2, params)
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

// TestTwoChainsAfterTheDataRules runs with the BLAKE2b node's data rules
// expired (TWOCHAIN_RDTS_EXPIRED=1, set by the script's RDTS_EXPIRED mode):
// a lock's data output no longer keeps it off the BLAKE2b chain. An
// unanchored lock of a twin left unspent there can then be mined there;
// a lock chained behind a permanent one (its anchor from a lock whose twin
// was passed on) cannot.
func TestTwoChainsAfterTheDataRules(t *testing.T) {
	if os.Getenv("TWOCHAIN_RDTS_EXPIRED") == "" {
		t.Skip("run by deploy/regtest/sha256-donations-twochain.sh with RDTS_EXPIRED=1")
	}
	ctx := context.Background()
	b2b, sha := client(t, os.Getenv("TWOCHAIN_BLAKE2B"), ""), client(t, os.Getenv("TWOCHAIN_SHA256"), "")
	donor, miner := client(t, os.Getenv("TWOCHAIN_SHA256"), "donor"), client(t, os.Getenv("TWOCHAIN_SHA256"), "miner")
	params := &chaincfg.RegressionNetParams
	seed, _ := hex.DecodeString(vectorSeed)
	keys, _ := NewKeys(seed, params)
	var minerAddr string
	call(t, miner, "getnewaddress", nil, &minerAddr)
	var mined []string

	// Three donations to request addresses 0, 1, 2, copied over by the
	// network, confirmed on both chains
	type donation struct {
		coin Coin
		raw  string
	}
	var ds []donation
	for i := uint32(0); i < 3; i++ {
		addr, script, _ := keys.Address(i)
		var id string
		call(t, donor, "sendtoaddress", []any{addr, 1.0}, &id)
		var got struct {
			Hex string `json:"hex"`
		}
		call(t, donor, "gettransaction", []any{id}, &got)
		tx := decodeHex(t, got.Hex)
		for v, out := range tx.TxOut {
			if bytes.Equal(out.PkScript, script) {
				ds = append(ds, donation{Coin{Txid: id, Vout: uint32(v), ValueSat: out.Value, KeyIndex: i}, got.Hex})
			}
		}
	}
	call(t, sha, "generatetoaddress", []any{1, minerAddr}, &mined)
	for _, d := range ds {
		if _, err := b2b.Send(ctx, d.raw); err != nil {
			t.Fatalf("the copy: %v", err)
		}
	}
	call(t, b2b, "generatetoaddress", []any{6, minerAddr}, &mined)

	// The danger: donation 0 locked without an anchor; its twin unspent on
	// the BLAKE2b chain. With the data rules gone, the BLAKE2b chain mines it
	l0, err := SignLock(keys, []Coin{ds[0].coin}, 0, nil, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sha.Send(ctx, l0.Raw); err != nil {
		t.Fatal(err)
	}
	var block any
	if err := b2b.Call(ctx, "generateblock", []any{minerAddr, []string{l0.Raw}}, &block); err != nil {
		t.Fatalf("expected the BLAKE2b chain to mine an unanchored lock once the data rules end: %v", err)
	}
	t.Log("without its data rules, the BLAKE2b chain mines an unanchored lock of an unspent twin")

	// The fix: donation 1 locked (lock 1), its twin passed on, so lock 1
	// can never be on the BLAKE2b chain; donation 2 locked behind lock 1's
	// anchor (lock 2). Its own twin is unspent there, yet lock 2 cannot be
	// mined: its anchor does not exist there
	l1, err := SignLock(keys, []Coin{ds[1].coin}, 1, nil, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sha.Send(ctx, l1.Raw); err != nil {
		t.Fatal(err)
	}
	call(t, sha, "generatetoaddress", []any{6, minerAddr}, &mined)
	var ret string
	call(t, miner, "getnewaddress", nil, &ret)
	f1, err := Sign(keys, []Coin{ds[1].coin}, ret, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b2b.Send(ctx, f1.Raw); err != nil {
		t.Fatal(err)
	}
	call(t, b2b, "generatetoaddress", []any{6, minerAddr}, &mined)

	anchor := Coin{Txid: l1.Txid, Vout: 1, ValueSat: AnchorSat, KeyIndex: 1, Branch: Locks}
	l2, err := SignLock(keys, []Coin{ds[2].coin}, 2, &anchor, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := sha.TestAccept(ctx, l2.Raw); err != nil || !acc.Allowed {
		t.Fatalf("the SHA256 node on the anchored lock: %+v %v", acc, err)
	}
	if c, _ := b2b.TxOut(ctx, ds[2].coin.Txid, ds[2].coin.Vout, true); c == nil {
		t.Fatal("donation 2's twin is not unspent on the BLAKE2b chain")
	}
	for _, pkg := range [][]string{{l2.Raw}, {l1.Raw, l2.Raw}} {
		err := b2b.Call(ctx, "generateblock", []any{minerAddr, pkg}, &block)
		if err == nil {
			t.Fatalf("the BLAKE2b chain mined the anchored lock (with %d transactions)", len(pkg))
		}
		t.Logf("the BLAKE2b chain refuses the anchored lock (%d tx): %v", len(pkg), err)
	}

	// The control: donation 2 locked without the anchor would be mined
	bare, err := SignLock(keys, []Coin{ds[2].coin}, 3, nil, 2, params)
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := b2b.TestAccept(ctx, bare.Raw); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("the same lock without the anchor, on the BLAKE2b node's mempool: allowed=%v %s", acc.Allowed, acc.Reason)
	}
	if err := b2b.Call(ctx, "generateblock", []any{minerAddr, []string{bare.Raw}}, &block); err != nil {
		t.Fatalf("control: the unanchored lock of donation 2 should be minable: %v", err)
	}
}
