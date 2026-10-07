package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// errChainMoved: the node's chain changed under a sync, which has undone
// what it could not trust and should run again.
var errChainMoved = errors.New("the chain changed during the sync")

type blockchainInfo struct {
	Blocks      int64 `json:"blocks"`
	Pruned      bool  `json:"pruned"`
	PruneHeight int64 `json:"pruneheight"`
}

type indexer struct {
	rpc    *rpcClient
	ix     *index
	window int64

	// mu serialises syncs: the poll loop and lookups that missed both run
	// them.
	mu sync.Mutex

	// When a sync last reached the node's tip, in Unix seconds; read by
	// /healthz and the poll loop
	lastSynced atomicTime
}

type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }

// catchUp syncs when the node's best block is not the index's tip. It costs
// one RPC when there is nothing new, so lookups for transactions nobody has
// do not each become a sync.
func (i *indexer) catchUp(ctx context.Context) error {
	var best string
	if err := i.rpc.call(ctx, "getbestblockhash", nil, &best); err != nil {
		return err
	}
	tip, err := i.ix.tip()
	if err != nil {
		return err
	}
	if stored, err := i.ix.blockAt(tip); err == nil && stored == best {
		return nil
	}
	// A sync that is running is usually the poll indexing that very block,
	// so wait for it, but not for minutes (the first sync after a start):
	// the caller then gets the node's own answer
	deadline := time.Now().Add(catchUpWait)
	for !i.mu.TryLock() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer i.mu.Unlock()
	return i.syncLocked(ctx)
}

// How long a lookup that missed waits for a sync that is running
var catchUpWait = 10 * time.Second

// sync brings the index to the node's tip: undoes any blocks the node no
// longer has in its active chain, indexes new ones, and drops what has left
// the window.
func (i *indexer) sync(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.syncLocked(ctx)
}

func (i *indexer) syncLocked(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		err := i.syncOnce(ctx)
		if !errors.Is(err, errChainMoved) || attempt == 4 {
			if err == nil {
				i.lastSynced.set(time.Now())
			}
			return err
		}
		log.Printf("%v; syncing again", err)
	}
}

func (i *indexer) syncOnce(ctx context.Context) error {
	var info blockchainInfo
	if err := i.rpc.call(ctx, "getblockchaininfo", nil, &info); err != nil {
		return err
	}

	tip, err := i.ix.tip()
	if err != nil {
		return err
	}
	if tip > info.Blocks {
		log.Printf("node is at %d, below the index tip %d: dropping the rest", info.Blocks, tip)
		if err := i.ix.removeFrom(info.Blocks + 1); err != nil {
			return err
		}
		tip = info.Blocks
	}

	// Walk back while what the index holds is not the node's block at that
	// height any more.
	for tip >= 0 {
		stored, err := i.ix.blockAt(tip)
		if err != nil {
			return err
		}
		if stored == "" {
			break
		}
		var nodeHash string
		if err := i.rpc.call(ctx, "getblockhash", []any{tip}, &nodeHash); err != nil {
			return err
		}
		if nodeHash == stored {
			break
		}
		log.Printf("reorganisation: block %d is now %s, was %s", tip, nodeHash, stored)
		if err := i.ix.removeFrom(tip); err != nil {
			return err
		}
		tip--
	}

	first := info.Blocks - i.window + 1
	if info.Pruned && info.PruneHeight > first {
		first = info.PruneHeight
	}
	if first < 0 {
		first = 0
	}
	start := tip + 1
	if start < first {
		start = first
	}

	for h := start; h <= info.Blocks; h++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var hash string
		if err := i.rpc.call(ctx, "getblockhash", []any{h}, &hash); err != nil {
			return err
		}
		var block struct {
			Tx                []string `json:"tx"`
			PreviousBlockHash string   `json:"previousblockhash"`
		}
		if err := i.rpc.call(ctx, "getblock", []any{hash, 1}, &block); err != nil {
			return err
		}
		// The block must build on the one stored below it: if the node
		// reorganised since the walk back above, the stored one is stale.
		below, err := i.ix.blockAt(h - 1)
		if err != nil {
			return err
		}
		if below != "" && below != block.PreviousBlockHash {
			log.Printf("reorganisation during sync: block %d builds on %s, the index has %s", h, block.PreviousBlockHash, below)
			if err := i.ix.removeFrom(h - 1); err != nil {
				return err
			}
			return errChainMoved
		}
		if err := i.ix.addBlock(h, hash, block.Tx); err != nil {
			return err
		}
		if (h-start)%500 == 499 {
			log.Printf("indexed to %d of %d", h, info.Blocks)
		}
	}

	return i.ix.removeBelow(first)
}

// inActiveChain asks the node whether a block is in its active chain (a
// block that left it has confirmations -1).
func (i *indexer) inActiveChain(ctx context.Context, blockHash string) (bool, error) {
	var header struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := i.rpc.call(ctx, "getblockheader", []any{blockHash, true}, &header); err != nil {
		return false, err
	}
	return header.Confirmations >= 0, nil
}
