// Package lnd reads the channel graph over lnd's REST API, with a
// macaroon that may only read it (graph.macaroon: GetInfo, DescribeGraph).
package lnd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client talks to lnd's REST API, trusting only its certificate.
type Client struct {
	URL      string
	macaroon string
	http     *http.Client
}

// New reads the certificate and the macaroon.
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
	return &Client{URL: url, macaroon: hex.EncodeToString(mac), http: &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}}, nil
}

func (c *Client) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lnd: HTTP %d: %.200s", res.StatusCode, b)
	}
	return b, nil
}

// Info is our node's key and how to reach it.
type Info struct {
	IdentityPubkey string   `json:"identity_pubkey"`
	URIs           []string `json:"uris"`
}

// GetInfo is our node.
func (c *Client) GetInfo(ctx context.Context) (Info, error) {
	var out Info
	b, err := c.get(ctx, "/v1/getinfo", 1<<20)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}

// DescribeGraph is the public channel graph, as JSON.
func (c *Client) DescribeGraph(ctx context.Context) ([]byte, error) {
	return c.get(ctx, "/v1/graph?include_unannounced=false", 512<<20)
}
