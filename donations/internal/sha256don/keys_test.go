package sha256don

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

// BIP39's "abandon ... about", no passphrase: BIP86's test vectors
const vectorSeed = "5eb00bbddcf069084889a8ab9155568165f5c453ccb85e70811aaed6f6da5fc1" +
	"9a5ac40b389cd370d086206dec8aa6c43daea6690f20ad3d8d48b2d2ce9e38e4"

func vectorKeys(t *testing.T) *Keys {
	t.Helper()
	seed, _ := hex.DecodeString(vectorSeed)
	k, err := NewKeys(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestBIP86Vectors(t *testing.T) {
	k := vectorKeys(t)
	for i, want := range []string{
		"bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr",
		"bc1p4qhjn9zdvkux4e44uhx8tc55attvtyu358kutcqkudyccelu0was9fqzwh",
	} {
		got, script, err := k.Address(uint32(i))
		if err != nil || got != want {
			t.Fatalf("address %d: %s %v", i, got, err)
		}
		if len(script) != 34 || script[0] != 0x51 {
			t.Fatalf("script %x", script)
		}
	}
	pub, err := k.Descriptor(false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub, "tr([73c5da0a/86h/0h/0h]xpub6BgBgsespWvERF3LHQu6CnqdvfEvtMcQjYrcRzx53QJjSxarj2afYWcLteoGVky7D3UKDP9QyrLprQ3VCECoY49yfdDEHGCtMMj92pReUsQ/0/*)#") {
		t.Fatalf("public descriptor %s", pub)
	}
	priv, _ := k.Descriptor(true)
	if !strings.Contains(priv, "xprv9xgqHN7yz9MwCkxsBPN5qetuNdQSUttZNKw1dcYTV4mkaAFiBVGQziHs3NRSWMkCzvgjEe3n9xV8oYywvM8at9yRqyaZVz6TYYhX98VjsUk/0/*") {
		t.Fatalf("private descriptor %s", priv)
	}
}

func TestDescriptorChecksum(t *testing.T) {
	// BIP380's example
	got, err := DescriptorChecksum("raw(deadbeef)")
	if err != nil || got != "89f8spxm" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := DescriptorChecksum("raw(é)"); err == nil {
		t.Fatal("a character outside the set was taken")
	}
}
