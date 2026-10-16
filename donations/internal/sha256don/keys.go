// Package sha256don runs donations on the SHA256 chain: each request gets
// an address of its own; what the SHA256 chain pays it is kept, and what
// reaches it on the BLAKE2b chain (a copy of the donation, or a plain send)
// is passed on, signed with SIGHASH_UNIFIED so that the passing on can't be
// copied back to the SHA256 chain, where it would spend the donation.
package sha256don

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// Keys derives the requests' keys: BIP86, m/86'/0'/0'/0/i. The same keys
// hold the coins on both chains.
type Keys struct {
	account *hdkeychain.ExtendedKey // m/86'/0'/0'
	chain   *hdkeychain.ExtendedKey // m/86'/0'/0'/0
	master  *hdkeychain.ExtendedKey
	params  *chaincfg.Params
}

// NewKeys derives from a seed (16 to 64 bytes).
func NewKeys(seed []byte, params *chaincfg.Params) (*Keys, error) {
	master, err := hdkeychain.NewMaster(seed, params)
	if err != nil {
		return nil, err
	}
	account := master
	for _, i := range []uint32{86, 0, 0} {
		if account, err = account.Derive(hdkeychain.HardenedKeyStart + i); err != nil {
			return nil, err
		}
	}
	chain, err := account.Derive(0)
	if err != nil {
		return nil, err
	}
	return &Keys{account: account, chain: chain, master: master, params: params}, nil
}

// LoadKeys reads the seed file: hex, one line.
func LoadKeys(path string, params *chaincfg.Params) (*Keys, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, errors.New("the seed file is not hex")
	}
	return NewKeys(seed, params)
}

// Private is the key of address i.
func (k *Keys) Private(i uint32) (*btcec.PrivateKey, error) {
	child, err := k.chain.Derive(i)
	if err != nil {
		return nil, err
	}
	return child.ECPrivKey()
}

// Address is address i (taproot, key path only) and its output script.
func (k *Keys) Address(i uint32) (string, []byte, error) {
	priv, err := k.Private(i)
	if err != nil {
		return "", nil, err
	}
	output := txscript.ComputeTaprootKeyNoScript(priv.PubKey())
	addr, err := btcutil.NewAddressTaproot(output.SerializeCompressed()[1:], k.params)
	if err != nil {
		return "", nil, err
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return "", nil, err
	}
	return addr.EncodeAddress(), script, nil
}

// Descriptor is the wallet as an output descriptor for another wallet
// (Sparrow, Knots): with the private key when private, else the public one.
func (k *Keys) Descriptor(private bool) (string, error) {
	fp, err := k.fingerprint()
	if err != nil {
		return "", err
	}
	key := k.account
	if !private {
		if key, err = k.account.Neuter(); err != nil {
			return "", err
		}
	}
	body := fmt.Sprintf("tr([%s/86h/0h/0h]%s/0/*)", fp, key.String())
	sum, err := DescriptorChecksum(body)
	if err != nil {
		return "", err
	}
	return body + "#" + sum, nil
}

func (k *Keys) fingerprint() (string, error) {
	pub, err := k.master.ECPubKey()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(btcutil.Hash160(pub.SerializeCompressed())[:4]), nil
}

// DescriptorChecksum is BIP380's checksum.
func DescriptorChecksum(desc string) (string, error) {
	const input = "0123456789()[],'/*abcdefgh@:$%{}" +
		"IJKLMNOPQRSTUVWXYZ&+-.;<=>?!^_|~" +
		"ijklmnopqrstuvwxyzABCDEFGH`#\"\\ "
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	polymod := func(c uint64, val int) uint64 {
		c0 := c >> 35
		c = ((c & 0x7ffffffff) << 5) ^ uint64(val)
		for i, g := range []uint64{0xf5dee51989, 0xa9fdca3312, 0x1bab10e32d,
			0x3706b1677a, 0x644d626ffd} {
			if c0>>i&1 == 1 {
				c ^= g
			}
		}
		return c
	}
	c, cls, clscount := uint64(1), 0, 0
	for _, ch := range desc {
		pos := strings.IndexRune(input, ch)
		if pos < 0 {
			return "", fmt.Errorf("descriptor character %q", ch)
		}
		c = polymod(c, pos&31)
		cls = cls*3 + pos>>5
		if clscount++; clscount == 3 {
			c = polymod(c, cls)
			cls, clscount = 0, 0
		}
	}
	if clscount > 0 {
		c = polymod(c, cls)
	}
	for i := 0; i < 8; i++ {
		c = polymod(c, 0)
	}
	c ^= 1
	out := make([]byte, 8)
	for j := 0; j < 8; j++ {
		out[j] = charset[(c>>(5*(7-j)))&31]
	}
	return string(out), nil
}
