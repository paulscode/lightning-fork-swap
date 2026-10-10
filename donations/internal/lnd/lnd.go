// Package lnd talks to lnd over its REST API: the donation watcher reads
// on-chain transactions with a macaroon that may do nothing else; the
// channel donation worker and its guard use the calls in channels.go.
package lnd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Client talks to lnd's REST API.
type Client struct {
	URL      string
	macaroon string
	http     *http.Client
}

// New reads the TLS certificate and the macaroon from files. The
// certificate is the only one trusted for the connection.
func New(url, certPath, macaroonPath string) (*Client, error) {
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		return nil, fmt.Errorf("no certificate in %s", certPath)
	}
	mac, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, err
	}
	return &Client{
		URL:      url,
		macaroon: hex.EncodeToString(mac),
		// Each call has its own deadline (call, OpenChannel)
		http: &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs: pool, MinVersion: tls.VersionTLS12,
			}},
		},
	}, nil
}

// OutputDetail is one output of a transaction.
type OutputDetail struct {
	Address      string `json:"address"`
	OutputIndex  string `json:"output_index"`
	Amount       string `json:"amount"`
	IsOurAddress bool   `json:"is_our_address"`
}

// Transaction is one of the wallet's transactions.
type Transaction struct {
	TxHash           string         `json:"tx_hash"`
	NumConfirmations int64          `json:"num_confirmations"`
	BlockHeight      int64          `json:"block_height"`
	TimeStamp        string         `json:"time_stamp"`
	OutputDetails    []OutputDetail `json:"output_details"`
}

// Paid is what the transaction pays to one of `addresses`, in sat.
func (t Transaction) Paid(addresses map[string]bool) (string, int64) {
	var to string
	var total int64
	for _, out := range t.OutputDetails {
		if addresses[out.Address] {
			amount, err := strconv.ParseInt(out.Amount, 10, 64)
			if err != nil {
				continue
			}
			to = out.Address
			total += amount
		}
	}
	return to, total
}

// Transactions lists the wallet's transactions from a height on (and
// unconfirmed ones).
func (c *Client) Transactions(ctx context.Context, startHeight int64) ([]Transaction, error) {
	var out struct {
		Transactions []Transaction `json:"transactions"`
	}
	path := fmt.Sprintf("/v1/transactions?start_height=%d&end_height=-1", startHeight)
	if err := c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Transactions, nil
}

// CallTimeout bounds a call that has no deadline of its own.
const CallTimeout = 30 * time.Second

// Error is lnd's answer to a call that failed.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("lnd: HTTP %d: %.300s", e.Status, e.Message) }

// call sends one request; body (if any) as JSON; the answer into out.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CallTimeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = string(b)
		}
		return &Error{Status: res.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
