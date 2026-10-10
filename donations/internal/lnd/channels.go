package lnd

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Int reads lnd's 64-bit numbers, which its REST API sends as strings.
type Int int64

// UnmarshalJSON accepts "123" and 123.
func (i *Int) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*i = Int(v)
	return err
}

// OutPoint is a transaction output.
type OutPoint struct {
	TxidStr     string `json:"txid_str"`
	OutputIndex uint32 `json:"output_index"`
}

func (o OutPoint) String() string { return fmt.Sprintf("%s:%d", o.TxidStr, o.OutputIndex) }

// Utxo is one of the wallet's unspent outputs.
type Utxo struct {
	Address       string   `json:"address"`
	AmountSat     Int      `json:"amount_sat"`
	Outpoint      OutPoint `json:"outpoint"`
	Confirmations Int      `json:"confirmations"`
}

// Lease is an output leased (locked) by someone.
type Lease struct {
	ID       string   `json:"id"` // base64
	Outpoint OutPoint `json:"outpoint"`
	Value    Int      `json:"value"`
}

// Info is lnd's own state.
type Info struct {
	IdentityPubkey string `json:"identity_pubkey"`
	BlockHeight    int64  `json:"block_height"`
	SyncedToChain  bool   `json:"synced_to_chain"`
}

// Feature is one feature bit a node advertises.
type Feature struct {
	Name       string `json:"name"`
	IsRequired bool   `json:"is_required"`
	IsKnown    bool   `json:"is_known"`
}

// NodeAddress is where a node says it listens.
type NodeAddress struct {
	Network string `json:"network"`
	Addr    string `json:"addr"`
}

// Node is what the graph knows of a node.
type Node struct {
	Alias       string             `json:"alias"`
	Addresses   []NodeAddress      `json:"addresses"`
	Features    map[string]Feature `json:"features"`
	NumChannels int64              `json:"-"`
	Capacity    Int                `json:"-"`
}

// Peer is a connected node.
type Peer struct {
	PubKey   string             `json:"pub_key"`
	Address  string             `json:"address"`
	Features map[string]Feature `json:"features"`
}

// HasFeature reports whether a feature bit (or its pair) is advertised.
func HasFeature(features map[string]Feature, bit int) bool {
	pair := bit ^ 1
	_, a := features[strconv.Itoa(bit)]
	_, b := features[strconv.Itoa(pair)]
	return a || b
}

// Channel is an open channel.
type Channel struct {
	RemotePubkey string `json:"remote_pubkey"`
	ChannelPoint string `json:"channel_point"`
	Capacity     Int    `json:"capacity"`
	Active       bool   `json:"active"`
	Initiator    bool   `json:"initiator"`
}

// PendingChannel is a channel being opened.
type PendingChannel struct {
	RemoteNodePub string `json:"remote_node_pub"`
	ChannelPoint  string `json:"channel_point"`
	Capacity      Int    `json:"capacity"`
}

// ClosedChannel is a channel that was closed.
type ClosedChannel struct {
	RemotePubkey string `json:"remote_pubkey"`
	ChannelPoint string `json:"channel_point"`
	CloseType    string `json:"close_type"`
	CloseHeight  int64  `json:"close_height"`
}

// OpenRequest is what the worker asks lnd to open.
type OpenRequest struct {
	NodePubkey         string // hex
	LocalFundingAmount int64  // 0 with FundMax
	FundMax            bool
	SatPerVbyte        uint64
	Outpoints          []OutPoint
	Memo               string
}

// GetInfo is lnd's identity and sync state.
func (c *Client) GetInfo(ctx context.Context) (Info, error) {
	var out Info
	return out, c.call(ctx, http.MethodGet, "/v1/getinfo", nil, &out)
}

// NewAddress is a fresh taproot address of the wallet.
func (c *Client) NewAddress(ctx context.Context) (string, error) {
	var out struct {
		Address string `json:"address"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/newaddress?type=TAPROOT_PUBKEY", nil, &out)
	if err == nil && out.Address == "" {
		err = fmt.Errorf("lnd gave no address")
	}
	return out.Address, err
}

// ListUnspent is the wallet's unspent outputs with at least minConfs
// confirmations (0: also unconfirmed). Leased outputs are not listed.
func (c *Client) ListUnspent(ctx context.Context, minConfs int) ([]Utxo, error) {
	var out struct {
		Utxos []Utxo `json:"utxos"`
	}
	body := map[string]any{"min_confs": minConfs, "max_confs": 1 << 30}
	return out.Utxos, c.call(ctx, http.MethodPost, "/v2/wallet/utxos", body, &out)
}

// LeaseOutput locks an output for id, for seconds.
func (c *Client) LeaseOutput(ctx context.Context, id []byte, op OutPoint, seconds uint64) error {
	body := map[string]any{"id": base64.StdEncoding.EncodeToString(id),
		"outpoint": op, "expiration_seconds": strconv.FormatUint(seconds, 10)}
	return c.call(ctx, http.MethodPost, "/v2/wallet/utxos/lease", body, nil)
}

// ReleaseOutput unlocks an output leased under id.
func (c *Client) ReleaseOutput(ctx context.Context, id []byte, op OutPoint) error {
	body := map[string]any{"id": base64.StdEncoding.EncodeToString(id), "outpoint": op}
	return c.call(ctx, http.MethodPost, "/v2/wallet/utxos/release", body, nil)
}

// ListLeases is every current lease.
func (c *Client) ListLeases(ctx context.Context) ([]Lease, error) {
	var out struct {
		LockedUtxos []Lease `json:"locked_utxos"`
	}
	return out.LockedUtxos, c.call(ctx, http.MethodPost, "/v2/wallet/utxos/leases",
		map[string]any{}, &out)
}

// LeasedBy reports whether an output is leased under id: the guard's check.
func (c *Client) LeasedBy(ctx context.Context, id []byte, txid string, index uint32) (bool, error) {
	leases, err := c.ListLeases(ctx)
	if err != nil {
		return false, err
	}
	want := base64.StdEncoding.EncodeToString(id)
	for _, l := range leases {
		if l.ID == want && strings.EqualFold(l.Outpoint.TxidStr, txid) &&
			l.Outpoint.OutputIndex == index {
			return true, nil
		}
	}
	return false, nil
}

// FeeRate is lnd's estimate for confirmation within target blocks, in
// sat/vB (rounded up).
func (c *Client) FeeRate(ctx context.Context, target int) (uint64, error) {
	var out struct {
		SatPerKw Int `json:"sat_per_kw"`
	}
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v2/wallet/estimatefee/%d", target),
		nil, &out); err != nil {
		return 0, err
	}
	// 1 vbyte = 4 weight units
	return uint64((int64(out.SatPerKw)*4 + 999) / 1000), nil
}

// GetNodeInfo is what the graph knows of a node; ErrNoNode when nothing.
func (c *Client) GetNodeInfo(ctx context.Context, pubkey string) (Node, error) {
	var out struct {
		Node          Node `json:"node"`
		NumChannels   Int  `json:"num_channels"`
		TotalCapacity Int  `json:"total_capacity"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/graph/node/"+url.PathEscape(pubkey)+
		"?include_channels=false", nil, &out)
	out.Node.NumChannels, out.Node.Capacity = int64(out.NumChannels), out.TotalCapacity
	return out.Node, err
}

// ConnectPeer connects to a node; already connected is not an error.
func (c *Client) ConnectPeer(ctx context.Context, pubkey, host string, timeout int) error {
	body := map[string]any{"addr": map[string]string{"pubkey": pubkey, "host": host},
		"perm": false, "timeout": strconv.Itoa(timeout)}
	err := c.call(ctx, http.MethodPost, "/v1/peers", body, nil)
	if e, ok := err.(*Error); ok && strings.Contains(e.Message, "already connected") {
		return nil
	}
	return err
}

// ListPeers is the connected nodes.
func (c *Client) ListPeers(ctx context.Context) ([]Peer, error) {
	var out struct {
		Peers []Peer `json:"peers"`
	}
	return out.Peers, c.call(ctx, http.MethodGet, "/v1/peers", nil, &out)
}

// OpenChannel opens a channel and returns its channel point (txid:index)
// once the funding transaction is published.
func (c *Client) OpenChannel(ctx context.Context, r OpenRequest) (string, error) {
	key, err := hex.DecodeString(r.NodePubkey)
	if err != nil || len(key) != 33 {
		return "", fmt.Errorf("bad node key")
	}
	body := map[string]any{
		"node_pubkey":   base64.StdEncoding.EncodeToString(key),
		"sat_per_vbyte": strconv.FormatUint(r.SatPerVbyte, 10),
		"outpoints":     r.Outpoints,
		"memo":          r.Memo,
	}
	if r.FundMax {
		body["fund_max"] = true
	} else {
		body["local_funding_amount"] = strconv.FormatInt(r.LocalFundingAmount, 10)
	}
	var out struct {
		FundingTxidBytes string `json:"funding_txid_bytes"`
		FundingTxidStr   string `json:"funding_txid_str"`
		OutputIndex      uint32 `json:"output_index"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/channels", body, &out); err != nil {
		return "", err
	}
	txid := out.FundingTxidStr
	if txid == "" {
		b, err := base64.StdEncoding.DecodeString(out.FundingTxidBytes)
		if err != nil || len(b) != 32 {
			return "", fmt.Errorf("lnd gave no funding transaction")
		}
		// Bytes in the order of the wire; the id is shown reversed
		for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
		txid = hex.EncodeToString(b)
	}
	return fmt.Sprintf("%s:%d", txid, out.OutputIndex), nil
}

// PendingOpens is the channels being opened.
func (c *Client) PendingOpens(ctx context.Context) ([]PendingChannel, error) {
	var out struct {
		PendingOpenChannels []struct {
			Channel PendingChannel `json:"channel"`
		} `json:"pending_open_channels"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/channels/pending", nil, &out); err != nil {
		return nil, err
	}
	var chans []PendingChannel
	for _, p := range out.PendingOpenChannels {
		chans = append(chans, p.Channel)
	}
	return chans, nil
}

// ListChannels is the open channels.
func (c *Client) ListChannels(ctx context.Context) ([]Channel, error) {
	var out struct {
		Channels []Channel `json:"channels"`
	}
	return out.Channels, c.call(ctx, http.MethodGet, "/v1/channels", nil, &out)
}

// ClosedChannels is the channels that were closed.
func (c *Client) ClosedChannels(ctx context.Context) ([]ClosedChannel, error) {
	var out struct {
		Channels []ClosedChannel `json:"channels"`
	}
	return out.Channels, c.call(ctx, http.MethodGet, "/v1/channels/closed", nil, &out)
}
