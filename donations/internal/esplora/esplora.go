// Package esplora reads transactions and spends from Esplora-style
// explorers (mempool.guide for the BLAKE2b chain, mempool.space and
// blockstream.info for the SHA256 chain), trying each URL in turn.
package esplora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// ErrNotFound: every explorer that answered said the thing does not exist.
var ErrNotFound = errors.New("not found")

var txidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidTxid tells a transaction id (lowercase hex, 32 bytes).
func ValidTxid(txid string) bool { return txidPattern.MatchString(txid) }

// Client reads one chain from one or more explorers.
type Client struct {
	URLs []string
	HTTP *http.Client
	// At least this long between two requests: public explorers ban
	// addresses that ask too fast, and a donation can have many inputs
	Interval time.Duration

	mu   sync.Mutex
	next time.Time
}

// DefaultInterval paces requests to 4 a second.
const DefaultInterval = 250 * time.Millisecond

// New returns a client with a 20 s timeout per request, paced at
// DefaultInterval.
func New(urls ...string) *Client {
	return &Client{URLs: urls, HTTP: &http.Client{Timeout: 20 * time.Second},
		Interval: DefaultInterval}
}

// wait holds a request until its turn.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	c.next = at.Add(c.Interval)
	c.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

// Status is where a transaction is.
type Status struct {
	Confirmed   bool  `json:"confirmed"`
	BlockHeight int64 `json:"block_height"`
	BlockTime   int64 `json:"block_time"`
}

// Prevout is the output an input spends.
type Prevout struct {
	ScriptPubKey string `json:"scriptpubkey"`
	Type         string `json:"scriptpubkey_type"`
	Address      string `json:"scriptpubkey_address"`
	Value        int64  `json:"value"`
}

// Vin is an input.
type Vin struct {
	Txid       string   `json:"txid"`
	Vout       uint32   `json:"vout"`
	Prevout    *Prevout `json:"prevout"`
	ScriptSig  string   `json:"scriptsig"`
	Witness    []string `json:"witness"`
	IsCoinbase bool     `json:"is_coinbase"`
}

// Vout is an output.
type Vout struct {
	Address string `json:"scriptpubkey_address"`
	Value   int64  `json:"value"`
}

// Tx is a transaction as an explorer gives it.
type Tx struct {
	Txid   string `json:"txid"`
	Vin    []Vin  `json:"vin"`
	Vout   []Vout `json:"vout"`
	Status Status `json:"status"`
}

// Outspend is whether an output is spent, and by what.
type Outspend struct {
	Spent bool   `json:"spent"`
	Txid  string `json:"txid"`
}

const maxBody = 4 << 20

func (c *Client) get(ctx context.Context, path string, out any) error {
	var errs []error
	notFound := 0
	for _, base := range c.URLs {
		if err := c.wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
		res.Body.Close()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if res.StatusCode == http.StatusNotFound ||
			(res.StatusCode == http.StatusBadRequest &&
				len(body) < 200 && notFoundBody(body)) {
			notFound++
			continue
		}
		if res.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Errorf("%s: HTTP %d", base, res.StatusCode))
			continue
		}
		if err := json.Unmarshal(body, out); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", base, err))
			continue
		}
		return nil
	}
	if notFound > 0 && len(errs) == 0 {
		return ErrNotFound
	}
	if notFound > 0 {
		// Some said "not found", others failed: not certain
		errs = append(errs, fmt.Errorf("%d said not found", notFound))
	}
	return fmt.Errorf("explorers: %w", errors.Join(errs...))
}

// Esplora answers 400 "Transaction not found" for unknown txids on some
// paths, 404 on others
func notFoundBody(body []byte) bool {
	s := string(body)
	return s == "Transaction not found" || s == "Invalid hex string"
}

// Tx reads a transaction with its inputs' prevouts.
func (c *Client) Tx(ctx context.Context, txid string) (*Tx, error) {
	if !ValidTxid(txid) {
		return nil, fmt.Errorf("not a txid: %q", txid)
	}
	var tx Tx
	if err := c.get(ctx, "/tx/"+txid, &tx); err != nil {
		return nil, err
	}
	if tx.Txid != txid {
		return nil, fmt.Errorf("explorer answered another transaction")
	}
	return &tx, nil
}

// TxStatus reads where a transaction is; ErrNotFound if nowhere.
func (c *Client) TxStatus(ctx context.Context, txid string) (*Status, error) {
	if !ValidTxid(txid) {
		return nil, fmt.Errorf("not a txid: %q", txid)
	}
	var status Status
	if err := c.get(ctx, "/tx/"+txid+"/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// Outspend reads whether an output is spent. An output that does not
// exist is ErrNotFound (or, on some explorers, reported unspent: callers
// that need to know it exists ask TxStatus of its transaction too).
func (c *Client) Outspend(ctx context.Context, txid string, vout uint32) (*Outspend, error) {
	if !ValidTxid(txid) {
		return nil, fmt.Errorf("not a txid: %q", txid)
	}
	var out Outspend
	if err := c.get(ctx, "/tx/"+txid+"/outspend/"+strconv.FormatUint(uint64(vout), 10), &out); err != nil {
		return nil, err
	}
	if out.Spent && !ValidTxid(out.Txid) {
		return nil, fmt.Errorf("explorer gave a malformed spend")
	}
	return &out, nil
}
