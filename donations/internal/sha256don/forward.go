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
		_, script, err := keys.Address(c.KeyIndex)
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

	// The size with the witnesses to come: a 64-byte signature and its
	// hash type byte per input
	for _, in := range tx.TxIn {
		in.Witness = wire.TxWitness{make([]byte, 65)}
	}
	vsize := virtualSize(tx)
	if feeRate < 1 {
		feeRate = 1
	}
	fee := int64(math.Ceil(float64(vsize) * feeRate))
	if total-fee < MinForwardSat {
		return nil, ErrTooSmall
	}
	tx.TxOut[0].Value = total - fee

	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, c := range coins {
		priv, err := keys.Private(c.KeyIndex)
		if err != nil {
			return nil, err
		}
		witness, err := txscript.TaprootWitnessSignature(tx, sigHashes, i,
			c.ValueSat, scripts[i], HashType, priv)
		if err != nil {
			return nil, err
		}
		tx.TxIn[i].Witness = witness
	}
	if err := CheckUnified(tx, fetcher); err != nil {
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

func virtualSize(tx *wire.MsgTx) int64 {
	weight := int64(tx.SerializeSizeStripped())*3 + int64(tx.SerializeSize())
	return (weight + 3) / 4
}
