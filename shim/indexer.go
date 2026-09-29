package main

import (
	"context"
	"log"
)

type blockchainInfo struct {
	Blocks      int64 `json:"blocks"`
	Pruned      bool  `json:"pruned"`
	PruneHeight int64 `json:"pruneheight"`
}

type indexer struct {
	rpc    *rpcClient
	ix     *index
	window int64
}

// sync brings the index to the node's tip: undoes any blocks the node no
// longer has in its active chain, indexes new ones, and drops what has left
// the window.
func (i *indexer) sync(ctx context.Context) error {
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
			Tx []string `json:"tx"`
		}
		if err := i.rpc.call(ctx, "getblock", []any{hash, 1}, &block); err != nil {
			return err
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
