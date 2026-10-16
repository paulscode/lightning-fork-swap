// Package knots is a small client for the BLAKE2b chain's Knots node, for
// an RPC user limited to reading the chain and relaying transactions: no
// wallet call is used, nor allowed to the user it runs as.
package knots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Client calls Knots' JSON-RPC.
type Client struct {
	URL, User, Password string
	HTTP                *http.Client
}

// New returns a client with a 30 s timeout per call.
func New(url, user, password string) *Client {
	return &Client{URL: url, User: user, Password: password,
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is an error Knots answered with.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("knots: %s (%d)", e.Message, e.Code) }

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "1.0", "id": method, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("knots %s: HTTP %d", method, res.StatusCode)
	}
	if reply.Error != nil {
		return reply.Error
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(reply.Result))
	dec.UseNumber()
	return dec.Decode(out)
}

// Sats reads an amount in BTC, as Knots writes it, exactly.
func Sats(n json.Number) (int64, error) {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return 0, fmt.Errorf("not an amount: %q", n)
	}
	r.Mul(r, big.NewRat(100_000_000, 1))
	if !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
		return 0, fmt.Errorf("not an amount in sats: %q", n)
	}
	return r.Num().Int64(), nil
}

// TxOut is an unspent output.
type TxOut struct {
	ValueSat      int64
	Confirmations int64
	ScriptPubKey  string
	Address       string
}

// TxOut reads an output of the UTXO set (with the mempool's view when
// mempool is true); nil when it is spent or never existed.
func (c *Client) TxOut(ctx context.Context, txid string, vout uint32, mempool bool) (*TxOut, error) {
	var out *struct {
		Value         json.Number `json:"value"`
		Confirmations int64       `json:"confirmations"`
		ScriptPubKey  struct {
			Hex     string `json:"hex"`
			Address string `json:"address"`
		} `json:"scriptPubKey"`
	}
	if err := c.call(ctx, "gettxout", []any{txid, vout, mempool}, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	sats, err := Sats(out.Value)
	if err != nil {
		return nil, err
	}
	return &TxOut{ValueSat: sats, Confirmations: out.Confirmations,
		ScriptPubKey: out.ScriptPubKey.Hex, Address: out.ScriptPubKey.Address}, nil
}

// Accept is testmempoolaccept's verdict on one transaction.
type Accept struct {
	Txid    string `json:"txid"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reject-reason"`
}

// TestAccept asks whether the mempool would take a transaction, without
// relaying it.
func (c *Client) TestAccept(ctx context.Context, raw string) (Accept, error) {
	var out []Accept
	if err := c.call(ctx, "testmempoolaccept", []any{[]string{raw}}, &out); err != nil {
		return Accept{}, err
	}
	if len(out) != 1 {
		return Accept{}, errors.New("knots: testmempoolaccept gave no verdict")
	}
	return out[0], nil
}

// Send relays a transaction. One already in the mempool or the chain is
// not an error.
func (c *Client) Send(ctx context.Context, raw string) (string, error) {
	var txid string
	err := c.call(ctx, "sendrawtransaction", []any{raw}, &txid)
	var rpc *Error
	if errors.As(err, &rpc) && (strings.Contains(rpc.Message, "already in") ||
		strings.Contains(rpc.Message, "txn-already-known")) {
		return "", nil
	}
	return txid, err
}

// FeeRate is the estimate, in sat/vB, to confirm within blocks; 0 when
// Knots has none.
func (c *Client) FeeRate(ctx context.Context, blocks int) (float64, error) {
	var out struct {
		FeeRate *json.Number `json:"feerate"`
	}
	if err := c.call(ctx, "estimatesmartfee", []any{blocks}, &out); err != nil {
		return 0, err
	}
	if out.FeeRate == nil {
		return 0, nil
	}
	perKvB, err := Sats(*out.FeeRate)
	if err != nil {
		return 0, err
	}
	return float64(perKvB) / 1000, nil
}

// Height is the chain's height.
func (c *Client) Height(ctx context.Context) (int64, error) {
	var h int64
	return h, c.call(ctx, "getblockcount", nil, &h)
}
