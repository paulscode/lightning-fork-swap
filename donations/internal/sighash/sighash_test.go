package sighash

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// der is a real ECDSA signature in DER with the hash type appended.
func der(t *testing.T, hashType byte) string {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	sig := ecdsa.Sign(key, make([]byte, 32)).Serialize()
	return hex.EncodeToString(append(sig, hashType))
}

func schnorr(n int, last byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x11
	}
	if n == 65 {
		b[64] = last
	}
	return hex.EncodeToString(b)
}

// push makes a data push of a hex string.
func push(h string) string {
	n := len(h) / 2
	switch {
	case n <= 0x4b:
		return hex.EncodeToString([]byte{byte(n)}) + h
	case n <= 0xff:
		return "4c" + hex.EncodeToString([]byte{byte(n)}) + h
	default:
		return "4d" + hex.EncodeToString([]byte{byte(n), byte(n >> 8)}) + h
	}
}

const pubkey = "02" + "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func TestRead(t *testing.T) {
	control := "c0" + strings.Repeat("22", 32)
	script := "20" + strings.Repeat("33", 32) + "ac"
	p2wpkhRedeem := "0014" + strings.Repeat("44", 20)
	p2wshRedeem := "0020" + strings.Repeat("55", 32)
	multisig := "52" + "21" + pubkey + "21" + pubkey + "52ae"

	cases := []struct {
		name string
		in   Input
		want Protection
		n    int
	}{
		{"taproot key path, DEFAULT", Input{ScriptType: "v1_p2tr",
			Witness: []string{schnorr(64, 0)}}, Unprotected, 1},
		{"taproot key path, ALL|UNIFIED", Input{ScriptType: "v1_p2tr",
			Witness: []string{schnorr(65, 0x21)}}, Protected, 1},
		{"taproot key path, ALL", Input{ScriptType: "v1_p2tr",
			Witness: []string{schnorr(65, 0x01)}}, Unprotected, 1},
		{"taproot key path, a 65-byte DEFAULT is no signature", Input{
			ScriptType: "v1_p2tr", Witness: []string{schnorr(65, 0x00)}},
			Unknown, 0},
		{"taproot key path with an annex", Input{ScriptType: "v1_p2tr",
			Witness: []string{schnorr(65, 0x21), "50aa"}}, Protected, 1},
		{"taproot script path, one of two signatures unified", Input{
			ScriptType: "v1_p2tr", Witness: []string{
				schnorr(64, 0), schnorr(65, 0xa3), script, control}},
			Protected, 2},
		{"taproot script path, none unified", Input{ScriptType: "v1_p2tr",
			Witness: []string{schnorr(64, 0), "", script, control}},
			Unprotected, 1},
		{"taproot script path, a bad control block", Input{
			ScriptType: "v1_p2tr", Witness: []string{schnorr(64, 0),
				script, "c0aa"}}, Unknown, 0},
		{"P2WPKH, ALL", Input{ScriptType: "v0_p2wpkh",
			Witness: []string{der(t, 0x01), pubkey}}, Unprotected, 1},
		{"P2WPKH, ALL|UNIFIED", Input{ScriptType: "v0_p2wpkh",
			Witness: []string{der(t, 0x21), pubkey}}, Protected, 1},
		{"P2WPKH, not a signature", Input{ScriptType: "v0_p2wpkh",
			Witness: []string{"30aa", pubkey}}, Unknown, 0},
		{"P2WSH multisig, mixed", Input{ScriptType: "v0_p2wsh",
			Witness: []string{"", der(t, 0x01), der(t, 0x21), multisig}},
			Protected, 2},
		{"P2WSH multisig, none", Input{ScriptType: "v0_p2wsh",
			Witness: []string{"", der(t, 0x01), der(t, 0x81), multisig}},
			Unprotected, 2},
		{"P2PKH, ALL", Input{ScriptType: "p2pkh",
			ScriptSig: push(der(t, 0x01)) + push(pubkey)}, Unprotected, 1},
		{"P2PKH, ALL|UNIFIED", Input{ScriptType: "p2pkh",
			ScriptSig: push(der(t, 0x21)) + push(pubkey)}, Protected, 1},
		{"P2PKH, not pushes", Input{ScriptType: "p2pkh",
			ScriptSig: "ac" + push(pubkey)}, Unknown, 0},
		{"P2SH-P2WPKH, ALL", Input{ScriptType: "p2sh",
			ScriptSig: push(p2wpkhRedeem),
			Witness:   []string{der(t, 0x01), pubkey}}, Unprotected, 1},
		{"P2SH-P2WPKH, ALL|UNIFIED", Input{ScriptType: "p2sh",
			ScriptSig: push(p2wpkhRedeem),
			Witness:   []string{der(t, 0x21), pubkey}}, Protected, 1},
		{"P2SH-P2WSH multisig", Input{ScriptType: "p2sh",
			ScriptSig: push(p2wshRedeem),
			Witness:   []string{"", der(t, 0x01), der(t, 0x01), multisig}},
			Unprotected, 2},
		{"legacy P2SH multisig", Input{ScriptType: "p2sh",
			ScriptSig: "00" + push(der(t, 0x21)) + push(der(t, 0x01)) +
				push(multisig)}, Protected, 2},
		{"an unknown script type", Input{ScriptType: "op_return"},
			Unknown, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Read(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Protection != c.want || len(got.HashTypes) != c.n {
				t.Fatalf("got %s with %d types (%x), want %s with %d",
					got.Protection, len(got.HashTypes), got.HashTypes,
					c.want, c.n)
			}
		})
	}
}

func TestReadBadHex(t *testing.T) {
	if _, err := Read(Input{ScriptType: "v1_p2tr", Witness: []string{"zz"}}); err == nil {
		t.Fatal("bad witness hex accepted")
	}
	if _, err := Read(Input{ScriptType: "p2pkh", ScriptSig: "z"}); err == nil {
		t.Fatal("bad scriptSig hex accepted")
	}
}

func TestDERIsStrict(t *testing.T) {
	sig, _ := hex.DecodeString(der(t, 0x01))
	if _, ok := derHashType(sig); !ok {
		t.Fatal("a real signature was refused")
	}
	// A length that does not add up
	bad := append([]byte{}, sig...)
	bad[1]++
	if _, ok := derHashType(bad); ok {
		t.Fatal("a wrong total length was accepted")
	}
	// A public key is not a signature
	key, _ := hex.DecodeString(pubkey)
	if _, ok := derHashType(key); ok {
		t.Fatal("a public key was taken for a signature")
	}
}

func TestPushes(t *testing.T) {
	long := strings.Repeat("ab", 300)
	items, ok := pushes(mustHex("00" + push("aa") + push(strings.Repeat("cd", 80)) + push(long) + "52"))
	if !ok || len(items) != 5 || items[0] != nil || len(items[2]) != 80 ||
		len(items[3]) != 300 || items[4][0] != 2 {
		t.Fatalf("pushes: %v %d", ok, len(items))
	}
	if _, ok := pushes(mustHex("05aa")); ok {
		t.Fatal("a push past the end was accepted")
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
