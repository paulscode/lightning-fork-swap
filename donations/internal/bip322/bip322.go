// Package bip322 checks a signed message: BIP-322 "simple" signatures for
// SegWit and taproot addresses, and the classic signmessage format for
// legacy P2PKH addresses. A donor proves they own an input address this
// way before coins copied to the SHA256 chain are returned to them.
package bip322

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Verify reports whether signature (base64) signs message for address.
func Verify(address, message, signature string, params *chaincfg.Params) error {
	if params == nil {
		params = &chaincfg.MainNetParams
	}
	addr, err := btcutil.DecodeAddress(address, params)
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("signature is not base64: %w", err)
	}
	if pkh, ok := addr.(*btcutil.AddressPubKeyHash); ok && len(sig) == 65 {
		return verifyLegacy(pkh, message, sig, params)
	}
	return verifySimple(addr, message, sig)
}

// MessageHash is BIP-322's tagged hash of the message.
func MessageHash(message string) []byte {
	return chainhash.TaggedHash([]byte("BIP0322-signed-message"),
		[]byte(message))[:]
}

func verifySimple(addr btcutil.Address, message string, sig []byte) error {
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return err
	}
	witness, err := readWitness(sig)
	if err != nil {
		return err
	}

	toSpend := wire.NewMsgTx(0)
	toSpend.LockTime = 0
	scriptSig, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).
		AddData(MessageHash(message)).Script()
	if err != nil {
		return err
	}
	toSpend.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0xffffffff},
		SignatureScript:  scriptSig,
		Sequence:         0,
	})
	toSpend.AddTxOut(wire.NewTxOut(0, pkScript))

	toSign := wire.NewMsgTx(0)
	toSign.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: toSpend.TxHash(), Index: 0},
		Sequence:         0,
		Witness:          witness,
	})
	opReturn, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_RETURN).Script()
	toSign.AddTxOut(wire.NewTxOut(0, opReturn))

	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, 0)
	hashes := txscript.NewTxSigHashes(toSign, fetcher)
	engine, err := txscript.NewEngine(pkScript, toSign, 0,
		txscript.StandardVerifyFlags, nil, hashes, 0, fetcher)
	if err != nil {
		return err
	}
	if err := engine.Execute(); err != nil {
		return fmt.Errorf("signature does not verify: %w", err)
	}
	return nil
}

// readWitness reads a serialized witness stack (count, then each item
// with its length, as BIP-322 encodes it).
func readWitness(b []byte) (wire.TxWitness, error) {
	r := bytes.NewReader(b)
	count, err := wire.ReadVarInt(r, 0)
	if err != nil || count == 0 || count > 100 {
		return nil, errors.New("signature is not a witness stack")
	}
	witness := make(wire.TxWitness, count)
	for i := range witness {
		n, err := wire.ReadVarInt(r, 0)
		if err != nil || n > 10_000 {
			return nil, errors.New("signature is not a witness stack")
		}
		witness[i] = make([]byte, n)
		if _, err := r.Read(witness[i]); err != nil && n > 0 {
			return nil, errors.New("signature is not a witness stack")
		}
	}
	if r.Len() != 0 {
		return nil, errors.New("signature has extra bytes")
	}
	return witness, nil
}

func verifyLegacy(addr *btcutil.AddressPubKeyHash, message string, sig []byte, params *chaincfg.Params) error {
	var buf bytes.Buffer
	_ = wire.WriteVarString(&buf, 0, "Bitcoin Signed Message:\n")
	_ = wire.WriteVarString(&buf, 0, message)
	first := sha256.Sum256(buf.Bytes())
	hash := sha256.Sum256(first[:])
	pub, compressed, err := ecdsa.RecoverCompact(sig, hash[:])
	if err != nil {
		return fmt.Errorf("signature does not verify: %w", err)
	}
	var serialized []byte
	if compressed {
		serialized = pub.SerializeCompressed()
	} else {
		serialized = pub.SerializeUncompressed()
	}
	got, err := btcutil.NewAddressPubKeyHash(btcutil.Hash160(serialized), params)
	if err != nil {
		return err
	}
	if got.EncodeAddress() != addr.EncodeAddress() {
		return errors.New("signature does not verify: another key signed it")
	}
	return nil
}
