package sha256don

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Coin is an output we hold on the BLAKE2b chain, at one of our addresses.
type Coin struct {
	Txid     string `json:"txid"`
	Vout     uint32 `json:"vout"`
	ValueSat int64  `json:"valueSat"`
	KeyIndex uint32 `json:"keyIndex"`
	// Requests (an address given to a donor) or Locks (the hot wallet's)
	Branch uint32 `json:"branch"`
}

// Outpoint is "txid:vout".
func (c Coin) Outpoint() string { return fmt.Sprintf("%s:%d", c.Txid, c.Vout) }

// Signed is a transaction passing coins on, ready to relay.
type Signed struct {
	Txid      string
	Raw       string
	AmountSat int64
	FeeSat    int64
	VSize     int64
}

// MinForwardSat: what would arrive after the fee must be at least this, or
// the coins are kept (a forward that hardly pays for itself helps no one).
const MinForwardSat = 1000

// ErrTooSmall: the coins don't cover the fee and MinForwardSat.
var ErrTooSmall = errors.New("too little to pass on after the fee")

// HashType is how every input is signed: all of the transaction, with the
// BLAKE2b chain's opt-in, which the SHA256 chain refuses.
const HashType = txscript.SigHashAll | txscript.SigHashUnified

// Sign builds and signs a transaction sending coins, less the fee at
// feeRate sat/vB, to dest. It verifies every input under the BLAKE2b
// chain's rules and that each fails under the SHA256 chain's before
// returning it.
func Sign(keys *Keys, coins []Coin, dest string, feeRate float64, params *chaincfg.Params) (*Signed, error) {
	return sign(keys, coins, dest, feeRate, params, HashType, CheckUnified)
}

// SignSHA256 builds and signs a transaction for the SHA256 chain: the
// ordinary way (BIP341, default hash type), which that chain takes. It
// verifies every input under the SHA256 chain's rules. Whether the BLAKE2b
// chain could take it too depends on its inputs, not on its signatures:
// the caller makes sure one of them cannot exist there.
func SignSHA256(keys *Keys, coins []Coin, dest string, feeRate float64, params *chaincfg.Params) (*Signed, error) {
	return sign(keys, coins, dest, feeRate, params, txscript.SigHashDefault, CheckSHA256)
}

func sign(keys *Keys, coins []Coin, dest string, feeRate float64, params *chaincfg.Params,
	hashType txscript.SigHashType, check func(*wire.MsgTx, txscript.PrevOutputFetcher) error) (*Signed, error) {
	return signWith(keys, coins, dest, feeRate, params, hashType, check)
}

// signWith: sign, with extra (zero-value) outputs after the payment
func signWith(keys *Keys, coins []Coin, dest string, feeRate float64, params *chaincfg.Params,
	hashType txscript.SigHashType, check func(*wire.MsgTx, txscript.PrevOutputFetcher) error,
	extra ...*wire.TxOut) (*Signed, error) {
	if len(coins) == 0 {
		return nil, errors.New("nothing to send")
	}
	addr, err := btcutil.DecodeAddress(dest, params)
	if err != nil || !addr.IsForNet(params) {
		return nil, fmt.Errorf("not an address on this network: %q", dest)
	}
	destScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx(2)
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	scripts := make([][]byte, len(coins))
	var total int64
	for i, c := range coins {
		hash, err := chainhash.NewHashFromStr(c.Txid)
		if err != nil {
			return nil, fmt.Errorf("coin %d: %w", i, err)
		}
		if c.ValueSat <= 0 {
			return nil, fmt.Errorf("coin %s has no value", c.Outpoint())
		}
		_, script, err := keys.AddressAt(c.Branch, c.KeyIndex)
		if err != nil {
			return nil, err
		}
		scripts[i] = script
		op := wire.NewOutPoint(hash, c.Vout)
		in := wire.NewTxIn(op, nil, nil)
		// Replaceable, should the fee need raising
		in.Sequence = wire.MaxTxInSequenceNum - 2
		tx.AddTxIn(in)
		fetcher.AddPrevOut(*op, wire.NewTxOut(c.ValueSat, script))
		total += c.ValueSat
	}
	tx.AddTxOut(wire.NewTxOut(0, destScript))
	for _, out := range extra {
		tx.AddTxOut(out)
	}

	// The size with the witnesses to come: a 64-byte signature, and its
	// hash type byte unless it is the default
	sigLen := 65
	if hashType == txscript.SigHashDefault {
		sigLen = 64
	}
	for _, in := range tx.TxIn {
		in.Witness = wire.TxWitness{make([]byte, sigLen)}
	}
	vsize := virtualSize(tx)
	if feeRate < 1 {
		feeRate = 1
	}
	fee := int64(math.Ceil(float64(vsize) * feeRate))
	for _, out := range extra {
		total -= out.Value // what the extra outputs carry
	}
	if total-fee < MinForwardSat {
		return nil, ErrTooSmall
	}
	tx.TxOut[0].Value = total - fee

	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, c := range coins {
		priv, err := keys.PrivateAt(c.Branch, c.KeyIndex)
		if err != nil {
			return nil, err
		}
		witness, err := txscript.TaprootWitnessSignature(tx, sigHashes, i,
			c.ValueSat, scripts[i], hashType, priv)
		if err != nil {
			return nil, err
		}
		tx.TxIn[i].Witness = witness
	}
	if err := check(tx, fetcher); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, err
	}
	return &Signed{Txid: tx.TxHash().String(), Raw: hex.EncodeToString(buf.Bytes()),
		AmountSat: total - fee, FeeSat: fee, VSize: virtualSize(tx)}, nil
}

// CheckUnified verifies each input of a signed transaction twice: it must
// pass with the BLAKE2b chain's unified signature hash honoured, and fail
// without it (the SHA256 chain's rules), and its signature must carry the
// opt-in byte.
func CheckUnified(tx *wire.MsgTx, fetcher txscript.PrevOutputFetcher) error {
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	sha256Flags := txscript.StandardVerifyFlags &^ txscript.ScriptVerifyUnifiedSigHash
	for i, in := range tx.TxIn {
		prev := fetcher.FetchPrevOutput(in.PreviousOutPoint)
		if prev == nil {
			return fmt.Errorf("input %d: unknown coin", i)
		}
		if len(in.Witness) != 1 || len(in.Witness[0]) != 65 ||
			txscript.SigHashType(in.Witness[0][64]) != HashType {
			return fmt.Errorf("input %d: not signed with the unified hash type", i)
		}
		vm, err := txscript.NewEngine(prev.PkScript, tx, i,
			txscript.StandardVerifyFlags, nil, sigHashes, prev.Value, fetcher)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		if err := vm.Execute(); err != nil {
			return fmt.Errorf("input %d does not verify on the BLAKE2b chain: %w", i, err)
		}
		vm, err = txscript.NewEngine(prev.PkScript, tx, i,
			sha256Flags, nil, sigHashes, prev.Value, fetcher)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		if vm.Execute() == nil {
			return fmt.Errorf("input %d would also be valid on the SHA256 chain", i)
		}
	}
	return nil
}

// LockData is the lock's OP_RETURN payload: 100 bytes, which puts the
// output at 103 bytes, over the 83 the BLAKE2b chain's data rules (RDTS)
// allow, so the BLAKE2b chain's consensus refuses the transaction. It says
// so, for whoever looks.
var LockData = []byte(fmt.Sprintf("%-100s",
	"Lightning Fork Swap lock: invalid on the BLAKE2b chain by design (RDTS: OP_RETURN over 83 bytes)"))[:100]

// MaxRDTSDataScript is the longest OP_RETURN output the BLAKE2b chain
// takes while its data rules hold.
const MaxRDTSDataScript = 83

// AnchorSat is what a lock's anchor output carries: enough to be spent,
// passed on from lock to lock.
const AnchorSat = 1000

// SignLock builds and signs the lock of coins on the SHA256 chain, its
// outputs all at the hot wallet's lock address j: the coins less the fee
// (output 0), an anchor (output 1, AnchorSat) for the next lock to spend,
// and LockData (output 2), which the BLAKE2b chain's data rules refuse.
// anchor, when not nil, is the previous lock's anchor, spent as one more
// input: a lock can then exist on the BLAKE2b chain only if that one can,
// whatever the data rules. Signed the ordinary way and verified under the
// SHA256 chain's rules.
func SignLock(keys *Keys, coins []Coin, j uint32, anchor *Coin, feeRate float64, params *chaincfg.Params) (*Signed, error) {
	dest, script, err := keys.AddressAt(Locks, j)
	if err != nil {
		return nil, err
	}
	// Built by hand: NullDataScript stops at the old 80-byte relay limit
	data, err := txscript.NewScriptBuilder().AddOp(txscript.OP_RETURN).AddData(LockData).Script()
	if err != nil {
		return nil, err
	}
	check := func(tx *wire.MsgTx, f txscript.PrevOutputFetcher) error {
		if err := CheckSHA256(tx, f); err != nil {
			return err
		}
		if !BreaksRDTS(tx) {
			return errors.New("the lock has no output the BLAKE2b chain refuses")
		}
		return nil
	}
	all := coins
	if anchor != nil {
		all = append(append([]Coin{}, coins...), *anchor)
	}
	return signWith(keys, all, dest, feeRate, params, txscript.SigHashDefault, check,
		wire.NewTxOut(AnchorSat, script), wire.NewTxOut(0, data))
}

// BreaksRDTS: the transaction has an output the BLAKE2b chain's data rules
// refuse (an OP_RETURN over 83 bytes, any other script over 34), so it can
// never be in a block there while they hold.
func BreaksRDTS(tx *wire.MsgTx) bool {
	for _, out := range tx.TxOut {
		n := len(out.PkScript)
		if n == 0 {
			continue
		}
		if (out.PkScript[0] == txscript.OP_RETURN && n > MaxRDTSDataScript) ||
			(out.PkScript[0] != txscript.OP_RETURN && n > 34) {
			return true
		}
	}
	return false
}

// CheckSHA256 verifies each input of a signed transaction under the SHA256
// chain's rules, and that it is signed the ordinary way (no opt-in byte).
func CheckSHA256(tx *wire.MsgTx, fetcher txscript.PrevOutputFetcher) error {
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	sha256Flags := txscript.StandardVerifyFlags &^ txscript.ScriptVerifyUnifiedSigHash
	for i, in := range tx.TxIn {
		prev := fetcher.FetchPrevOutput(in.PreviousOutPoint)
		if prev == nil {
			return fmt.Errorf("input %d: unknown coin", i)
		}
		if len(in.Witness) != 1 || len(in.Witness[0]) != 64 {
			return fmt.Errorf("input %d: not a default-hash-type key spend", i)
		}
		vm, err := txscript.NewEngine(prev.PkScript, tx, i, sha256Flags, nil,
			sigHashes, prev.Value, fetcher)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		if err := vm.Execute(); err != nil {
			return fmt.Errorf("input %d does not verify on the SHA256 chain: %w", i, err)
		}
	}
	return nil
}

func virtualSize(tx *wire.MsgTx) int64 {
	weight := int64(tx.SerializeSizeStripped())*3 + int64(tx.SerializeSize())
	return (weight + 3) / 4
}
