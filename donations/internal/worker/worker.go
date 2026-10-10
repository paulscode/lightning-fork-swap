// Package worker finds donation transactions in lnd's wallet and checks
// each for replay protection, again on the schedule its verdict calls for.
package worker

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

// Wallet is the part of lnd the worker reads.
type Wallet interface {
	Transactions(ctx context.Context, startHeight int64) ([]lnd.Transaction, error)
}

// Worker holds what one pass needs.
type Worker struct {
	Store     store.Store
	Wallet    Wallet
	Blake2b   replay.Explorer
	Sha256    replay.Explorer
	Addresses map[string]bool
	// More addresses to watch, read each pass (channel donations' own)
	MoreAddresses func(ctx context.Context) ([]string, error)
	Now           func() time.Time
	Logger        *log.Logger
}

const (
	cursorKey = "lnd_start_height"
	// LastPassKey holds when a pass last completed (RFC 3339), for the
	// monitor to see a worker that stopped working
	LastPassKey = "last_pass"
)

// Pass finds new donations and checks the ones that are due. Errors of
// single checks are recorded on their rows; the pass returns the first
// error that kept it from reading lnd or the database.
func (w *Worker) Pass(ctx context.Context) error {
	if err := w.find(ctx); err != nil {
		return fmt.Errorf("finding donations: %w", err)
	}
	if err := w.checkDue(ctx); err != nil {
		return err
	}
	return w.Store.SetState(ctx, LastPassKey, w.Now().UTC().Format(time.RFC3339))
}

func (w *Worker) find(ctx context.Context) error {
	cursor, err := w.Store.State(ctx, cursorKey)
	if err != nil {
		return err
	}
	start, _ := strconv.ParseInt(cursor, 10, 64)
	txs, err := w.Wallet.Transactions(ctx, start)
	if err != nil {
		return err
	}
	addresses := w.Addresses
	if w.MoreAddresses != nil {
		// Without them (no channel donations table) the fixed ones still count
		if more, err := w.MoreAddresses(ctx); err == nil && len(more) > 0 {
			addresses = map[string]bool{}
			for a := range w.Addresses {
				addresses[a] = true
			}
			for _, a := range more {
				addresses[a] = true
			}
		}
	}
	newest := start
	for _, tx := range txs {
		if !esplora.ValidTxid(tx.TxHash) {
			continue
		}
		if to, amount := tx.Paid(addresses); amount > 0 {
			if err := w.Store.AddDonation(ctx, tx.TxHash, to, amount); err != nil {
				return err
			}
		}
		// Read again from a few blocks back: a reorganisation may move them
		if tx.NumConfirmations > 0 && tx.BlockHeight-6 > newest {
			newest = tx.BlockHeight - 6
		}
	}
	if newest != start {
		return w.Store.SetState(ctx, cursorKey, strconv.FormatInt(newest, 10))
	}
	return nil
}

func (w *Worker) checkDue(ctx context.Context) error {
	now := w.Now()
	due, err := w.Store.Due(ctx, now, 50)
	if err != nil {
		return err
	}
	for _, rec := range due {
		result, err := replay.Check(ctx, rec.Txid, w.Blake2b, w.Sha256)
		if err != nil {
			result = replay.Result{Verdict: replay.Unknown, Reason: err.Error()}
			if len(result.Reason) > 300 {
				result.Reason = result.Reason[:300]
			}
		}
		// A transaction once at risk stays reported as such while the
		// explorers cannot answer, rather than going back to "unknown"
		if result.Verdict == replay.Unknown && rec.Verdict == replay.AtRisk {
			result.Verdict = replay.AtRisk
			result.Inputs = rec.Inputs
			result.AtRiskAddresses = rec.AtRiskAddresses
		}
		next := replay.NextCheck(result.Verdict, rec.FirstSeen, now)
		if err := w.Store.Save(ctx, rec.Txid, result, now, next); err != nil {
			return err
		}
		if w.Logger != nil && result.Verdict != rec.Verdict {
			w.Logger.Printf("donation %s: %s (%s)", rec.Txid, result.Verdict, result.Reason)
		}
	}
	return nil
}

// Run passes every interval until the context ends.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := w.Pass(ctx); err != nil && w.Logger != nil {
			w.Logger.Printf("pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
