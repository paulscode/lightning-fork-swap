// Package sighash reads the signature hash types of a transaction input
// from its witness and scriptSig, to tell whether the input is bound to the
// Bitcoin (BLAKE2b) chain by SIGHASH_UNIFIED (bit 0x20 of the hash type) or
// is valid on the SHA256 chain too.
//
// A signature carrying the bit commits to a message only the BLAKE2b chain
// computes, so an input with one such signature (checked by its script) is
// invalid on the SHA256 chain. An input whose signatures all lack the bit
// is valid on both chains, if the coin it spends exists on both.
package sighash

import (
	"encoding/hex"
	"fmt"
)

// Unified is the SIGHASH_UNIFIED bit of a hash type.
const Unified = 0x20

// Protection is what an input's signatures say.
type Protection string

const (
	// Protected: a signature carries SIGHASH_UNIFIED.
	Protected Protection = "protected"
	// Unprotected: every signature found lacks it.
	Unprotected Protection = "unprotected"
	// Unknown: no signature could be read with confidence.
	Unknown Protection = "unknown"
)

// Input is the part of an input the reader needs, as an explorer gives it.
type Input struct {
	// The spent output's type: v1_p2tr, v0_p2wpkh, v0_p2wsh, p2pkh, p2sh
	ScriptType string
	// Witness stack items, hex
	Witness []string
	// scriptSig, hex
	ScriptSig string
}

// Result is the reading of one input.
type Result struct {
	Protection Protection
	// The hash types read, in order (DEFAULT taproot signatures as 0x00)
	HashTypes []byte
}

// Read reads the signature hash types of an input.
func Read(in Input) (Result, error) {
	witness := make([][]byte, len(in.Witness))
	for i, item := range in.Witness {
		b, err := hex.DecodeString(item)
		if err != nil {
			return Result{}, fmt.Errorf("witness item %d: %w", i, err)
		}
		witness[i] = b
	}
	scriptSig, err := hex.DecodeString(in.ScriptSig)
	if err != nil {
		return Result{}, fmt.Errorf("scriptSig: %w", err)
	}

	var types []byte
	switch in.ScriptType {
	case "v1_p2tr":
		types = taproot(witness)
	case "v0_p2wpkh":
		// [signature, pubkey]
		if len(witness) == 2 {
			if t, ok := derHashType(witness[0]); ok {
				types = []byte{t}
			}
		}
	case "v0_p2wsh":
		// Signatures among the items before the witness script
		if len(witness) >= 2 {
			types = derItems(witness[:len(witness)-1])
		}
	case "p2pkh":
		items, ok := pushes(scriptSig)
		if ok && len(items) == 2 {
			if t, ok := derHashType(items[0]); ok {
				types = []byte{t}
			}
		}
	case "p2sh":
		items, ok := pushes(scriptSig)
		switch {
		case !ok || len(items) == 0:
		case len(items) == 1 && len(witness) > 0:
			// P2SH-wrapped SegWit: the redeem script names the program
			redeem := items[0]
			switch {
			case len(redeem) == 22 && redeem[0] == 0x00 && redeem[1] == 0x14:
				if len(witness) == 2 {
					if t, ok := derHashType(witness[0]); ok {
						types = []byte{t}
					}
				}
			case len(redeem) == 34 && redeem[0] == 0x00 && redeem[1] == 0x20:
				if len(witness) >= 2 {
					types = derItems(witness[:len(witness)-1])
				}
			}
		default:
			// Legacy P2SH: signatures before the redeem script
			types = derItems(items[:len(items)-1])
		}
	}

	return Result{Protection: judge(types), HashTypes: types}, nil
}

func judge(types []byte) Protection {
	if len(types) == 0 {
		return Unknown
	}
	for _, t := range types {
		if t&Unified != 0 {
			return Protected
		}
	}
	return Unprotected
}

// taproot reads a taproot input's witness: a key-path spend is one
// signature (64 bytes: SIGHASH_DEFAULT; 65: the last byte is the type); a
// script-path spend ends in the script and the control block, and its
// signatures are the 64- and 65-byte items before them.
func taproot(witness [][]byte) []byte {
	// An annex (last item starting 0x50, with two or more items) is dropped
	if len(witness) >= 2 {
		last := witness[len(witness)-1]
		if len(last) > 0 && last[0] == 0x50 {
			witness = witness[:len(witness)-1]
		}
	}
	if len(witness) == 1 {
		if t, ok := schnorrHashType(witness[0]); ok {
			return []byte{t}
		}
		return nil
	}
	if len(witness) < 2 {
		return nil
	}
	control := witness[len(witness)-1]
	if len(control) < 33 || (len(control)-33)%32 != 0 {
		return nil
	}
	var types []byte
	for _, item := range witness[:len(witness)-2] {
		if t, ok := schnorrHashType(item); ok {
			types = append(types, t)
		}
	}
	return types
}

func schnorrHashType(sig []byte) (byte, bool) {
	switch len(sig) {
	case 64:
		return 0x00, true
	case 65:
		// A 65-byte signature with type 0x00 is invalid; it is not a type
		if sig[64] == 0x00 {
			return 0, false
		}
		return sig[64], true
	}
	return 0, false
}

// derItems reads the DER signatures among stack items; other items (empty
// placeholders, public keys, preimages) are skipped.
func derItems(items [][]byte) []byte {
	var types []byte
	for _, item := range items {
		if t, ok := derHashType(item); ok {
			types = append(types, t)
		}
	}
	return types
}

// derHashType reads the hash type after a DER-encoded ECDSA signature:
// 0x30, the length of what follows (minus the hash type byte), two
// integers.
func derHashType(sig []byte) (byte, bool) {
	if len(sig) < 9 || len(sig) > 73 || sig[0] != 0x30 ||
		int(sig[1]) != len(sig)-3 {
		return 0, false
	}
	// r
	if sig[2] != 0x02 {
		return 0, false
	}
	rLen := int(sig[3])
	if rLen == 0 || 4+rLen+2 > len(sig)-1 {
		return 0, false
	}
	// s
	sAt := 4 + rLen
	if sig[sAt] != 0x02 {
		return 0, false
	}
	sLen := int(sig[sAt+1])
	if sLen == 0 || sAt+2+sLen != len(sig)-1 {
		return 0, false
	}
	return sig[len(sig)-1], true
}

// pushes parses a scriptSig made only of data pushes (as standard
// scriptSigs are); ok is false for anything else.
func pushes(script []byte) ([][]byte, bool) {
	var items [][]byte
	for i := 0; i < len(script); {
		op := script[i]
		i++
		var n int
		switch {
		case op == 0x00:
			items = append(items, nil)
			continue
		case op >= 0x01 && op <= 0x4b:
			n = int(op)
		case op == 0x4c:
			if i+1 > len(script) {
				return nil, false
			}
			n = int(script[i])
			i++
		case op == 0x4d:
			if i+2 > len(script) {
				return nil, false
			}
			n = int(script[i]) | int(script[i+1])<<8
			i += 2
		case op >= 0x51 && op <= 0x60:
			items = append(items, []byte{op - 0x50})
			continue
		default:
			return nil, false
		}
		if i+n > len(script) {
			return nil, false
		}
		items = append(items, script[i:i+n])
		i += n
	}
	return items, true
}
