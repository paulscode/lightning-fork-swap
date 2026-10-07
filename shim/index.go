package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The index maps a transaction id to the hash of the block that confirmed
// it, for a window of recent blocks. It is kept per height as well, so a
// reorganisation or the window moving on can remove exactly what one block
// added.
var (
	bucketTx     = []byte("tx")     // txid (32) -> block hash (32)
	bucketHeight = []byte("height") // height (4, BE) -> block hash (32) || txids (32 each)
)

type index struct {
	db *bolt.DB
}

func openIndex(path string) (*index, error) {
	// A second shim on the same file would otherwise wait forever
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketTx, bucketHeight} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &index{db: db}, nil
}

func (ix *index) close() error { return ix.db.Close() }

func heightKey(h int64) []byte {
	var k [4]byte
	binary.BigEndian.PutUint32(k[:], uint32(h))
	return k[:]
}

func decodeHash(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("hash %q is not 32 bytes", s)
	}
	return b, nil
}

// addBlock records the transactions of the block at height, replacing
// whatever was recorded at that height before.
func (ix *index) addBlock(height int64, blockHash string, txids []string) error {
	bh, err := decodeHash(blockHash)
	if err != nil {
		return err
	}
	return ix.db.Update(func(tx *bolt.Tx) error {
		if err := removeHeight(tx, height); err != nil {
			return err
		}
		txb := tx.Bucket(bucketTx)
		val := make([]byte, 0, 32+32*len(txids))
		val = append(val, bh...)
		for _, id := range txids {
			t, err := decodeHash(id)
			if err != nil {
				return err
			}
			if err := txb.Put(t, bh); err != nil {
				return err
			}
			val = append(val, t...)
		}
		return tx.Bucket(bucketHeight).Put(heightKey(height), val)
	})
}

func removeHeight(tx *bolt.Tx, height int64) error {
	hb := tx.Bucket(bucketHeight)
	val := hb.Get(heightKey(height))
	if val == nil {
		return nil
	}
	if len(val) < 32 || (len(val)-32)%32 != 0 {
		return errors.New("corrupt height record")
	}
	blockHash := val[:32]
	txb := tx.Bucket(bucketTx)
	for off := 32; off < len(val); off += 32 {
		t := val[off : off+32]
		// Only remove the entry if it still points at this block: the same
		// transaction may have been recorded again in a later block after a
		// reorganisation.
		if cur := txb.Get(t); cur != nil && string(cur) == string(blockHash) {
			if err := txb.Delete(t); err != nil {
				return err
			}
		}
	}
	return hb.Delete(heightKey(height))
}

// removeFrom drops every height at or above from (a reorganisation).
func (ix *index) removeFrom(from int64) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketHeight).Cursor()
		var heights []int64
		for k, _ := c.Seek(heightKey(from)); k != nil; k, _ = c.Next() {
			heights = append(heights, int64(binary.BigEndian.Uint32(k)))
		}
		for _, h := range heights {
			if err := removeHeight(tx, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// removeBelow drops every height below before (the window moving on).
func (ix *index) removeBelow(before int64) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketHeight).Cursor()
		var heights []int64
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			h := int64(binary.BigEndian.Uint32(k))
			if h >= before {
				break
			}
			heights = append(heights, h)
		}
		for _, h := range heights {
			if err := removeHeight(tx, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// blockAt returns the block hash recorded at height, or "" if none.
func (ix *index) blockAt(height int64) (string, error) {
	var out string
	err := ix.db.View(func(tx *bolt.Tx) error {
		val := tx.Bucket(bucketHeight).Get(heightKey(height))
		if len(val) >= 32 {
			out = hex.EncodeToString(val[:32])
		}
		return nil
	})
	return out, err
}

// tip returns the highest recorded height, or -1 for an empty index.
func (ix *index) tip() (int64, error) {
	h := int64(-1)
	err := ix.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(bucketHeight).Cursor().Last()
		if k != nil {
			h = int64(binary.BigEndian.Uint32(k))
		}
		return nil
	})
	return h, err
}

// lookup returns the hash of the block that confirmed txid, or "".
func (ix *index) lookup(txid string) (string, error) {
	t, err := decodeHash(txid)
	if err != nil {
		return "", nil // not a txid: nothing to add
	}
	var out string
	err = ix.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketTx).Get(t); v != nil {
			out = hex.EncodeToString(v)
		}
		return nil
	})
	return out, err
}

func (ix *index) count() (int, error) {
	n := 0
	err := ix.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketTx).Stats().KeyN
		return nil
	})
	return n, err
}
