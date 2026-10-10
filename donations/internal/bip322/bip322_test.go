package bip322

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
)

// The test vectors of BIP-322
const (
	p2wpkh = "bc1q9vza2e8x573nczrlzms0wvx3gsqjx7vavgkx0l"
	p2tr   = "bc1ppv609nr0vr25u07u95waq5lucwfm6tde4nydujnu8npg4q75mr5sxq8lt3"
)

func TestMessageHash(t *testing.T) {
	for msg, want := range map[string]string{
		"":            "c90c269c4f8fcbe6880f72a721ddfbf1914268a794cbb21cfafee13770ae19f1",
		"Hello World": "f0eb03b1a75ac6d9847f55c624a99169b5dccba2a31f5b23bea77ba270de0a7a",
	} {
		if got := hex.EncodeToString(MessageHash(msg)); got != want {
			t.Errorf("%q: %s", msg, got)
		}
	}
}

func TestBIP322Vectors(t *testing.T) {
	cases := []struct{ address, message, signature string }{
		{p2wpkh, "", "AkcwRAIgM2gBAQqvZX15ZiysmKmQpDrG83avLIT492QBzLnQIxYCIBaTpOaD20qRlEylyxFSeEA2ba9YOixpX8z46TSDtS40ASECx/EgAxlkQpQ9hYjgGu6EBCPMVPwVIVJqO4XCsMvViHI="},
		{p2wpkh, "Hello World", "AkcwRAIgZRfIY3p7/DoVTty6YZbWS71bc5Vct9p9Fia83eRmw2QCICK/ENGfwLtptFluMGs2KsqoNSk89pO7F29zJLUx9a/sASECx/EgAxlkQpQ9hYjgGu6EBCPMVPwVIVJqO4XCsMvViHI="},
		{p2tr, "Hello World", "AUHd69PrJQEv+oKTfZ8l+WROBHuy9HKrbFCJu7U1iK2iiEy1vMU5EfMtjc+VSHM7aU0SDbak5IUZRVno2P5mjSafAQ=="},
	}
	for _, c := range cases {
		if err := Verify(c.address, c.message, c.signature, nil); err != nil {
			t.Errorf("%s %q: %v", c.address, c.message, err)
		}
		if err := Verify(c.address, c.message+"!", c.signature, nil); err == nil {
			t.Errorf("%s: another message verified", c.address)
		}
	}
	// A signature of one address does not verify for another
	if err := Verify(p2tr, "Hello World", cases[1].signature, nil); err == nil {
		t.Error("a P2WPKH signature verified for a taproot address")
	}
}

func TestLegacySignMessage(t *testing.T) {
	key, _ := btcec.NewPrivateKey()
	addr, _ := btcutil.NewAddressPubKeyHash(
		btcutil.Hash160(key.PubKey().SerializeCompressed()), &chaincfg.MainNetParams)
	sign := func(message string) string {
		var buf bytes.Buffer
		_ = wire.WriteVarString(&buf, 0, "Bitcoin Signed Message:\n")
		_ = wire.WriteVarString(&buf, 0, message)
		first := sha256.Sum256(buf.Bytes())
		hash := sha256.Sum256(first[:])
		sig := ecdsa.SignCompact(key, hash[:], true)
		return base64.StdEncoding.EncodeToString(sig)
	}
	msg := "return to bc1qmine"
	if err := Verify(addr.EncodeAddress(), msg, sign(msg), nil); err != nil {
		t.Fatal(err)
	}
	if err := Verify(addr.EncodeAddress(), msg, sign("other"), nil); err == nil {
		t.Fatal("another message verified")
	}
	other, _ := btcec.NewPrivateKey()
	otherAddr, _ := btcutil.NewAddressPubKeyHash(
		btcutil.Hash160(other.PubKey().SerializeCompressed()), &chaincfg.MainNetParams)
	if err := Verify(otherAddr.EncodeAddress(), msg, sign(msg), nil); err == nil {
		t.Fatal("verified for another key")
	}
}

func TestMalformed(t *testing.T) {
	for _, c := range []struct{ address, signature string }{
		{"not-an-address", "AA=="},
		{p2wpkh, "%%%"},
		{p2wpkh, base64.StdEncoding.EncodeToString([]byte{0})},
		{p2wpkh, base64.StdEncoding.EncodeToString([]byte{1, 5, 1})},
	} {
		if err := Verify(c.address, "m", c.signature, nil); err == nil {
			t.Errorf("%q %q verified", c.address, c.signature)
		}
	}
}
