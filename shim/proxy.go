package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// The largest request body forwarded; Knots' own limit is far larger, but
// nothing the backend sends comes close.
const maxBody = 32 << 20

// Bitcoin Core's "No such mempool or blockchain transaction", returned by
// getrawtransaction without -txindex for anything outside the mempool.
const rpcInvalidAddressOrKey = -5

type proxy struct {
	upstream string
	ix       *index
	http     *http.Client

	// catchUp, when set, brings the index to the node's tip. A lookup that
	// misses runs it once and looks again: a block can arrive between two
	// polls, and a caller reacting to that block asks at once.
	catchUp func(context.Context) error

	// inActiveChain, when set, says whether a block is still in the node's
	// active chain. An index entry whose block left it (a reorganisation
	// the index has not caught up with) is not used.
	inActiveChain func(ctx context.Context, blockHash string) (bool, error)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rawResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     json.RawMessage `json:"id"`
}

// target is the upstream URL with the caller's path and query: wallet calls
// go to /wallet/<name>, and must reach that wallet.
func (p *proxy) target(r *http.Request) (string, error) {
	u, err := url.Parse(p.upstream)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + r.URL.Path
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	return u.String(), nil
}

func (p *proxy) forward(r *http.Request, body []byte) (int, []byte, error) {
	target, err := p.target(r)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for _, h := range []string{"Authorization", "Content-Type"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// withBlockHash returns req rewritten to name the block that confirmed its
// transaction, when req is a getrawtransaction that names none and the index
// knows the block. ok is false otherwise.
func (p *proxy) withBlockHash(ctx context.Context, req rpcRequest) (rpcRequest, bool) {
	if req.Method != "getrawtransaction" || len(req.Params) == 0 {
		return req, false
	}

	var txid string
	var verbose any = false
	var named map[string]json.RawMessage
	var positional []json.RawMessage

	switch {
	case json.Unmarshal(req.Params, &positional) == nil:
		if len(positional) == 0 || len(positional) > 2 {
			return req, false
		}
		if json.Unmarshal(positional[0], &txid) != nil {
			return req, false
		}
		if len(positional) == 2 && json.Unmarshal(positional[1], &verbose) != nil {
			return req, false
		}
	case json.Unmarshal(req.Params, &named) == nil:
		if _, has := named["blockhash"]; has {
			return req, false
		}
		if json.Unmarshal(named["txid"], &txid) != nil {
			return req, false
		}
		if v, has := named["verbose"]; has && json.Unmarshal(v, &verbose) != nil {
			return req, false
		}
		if v, has := named["verbosity"]; has && json.Unmarshal(v, &verbose) != nil {
			return req, false
		}
	default:
		return req, false
	}

	blockHash, err := p.lookupActive(ctx, txid)
	if err == nil && blockHash == "" && p.catchUp != nil {
		if err := p.catchUp(ctx); err != nil {
			log.Printf("catching up for %s: %v", txid, err)
		}
		blockHash, err = p.lookupActive(ctx, txid)
	}
	if err != nil {
		log.Printf("index lookup %s: %v", txid, err)
		return req, false
	}
	if blockHash == "" {
		return req, false
	}

	params, err := json.Marshal([]any{txid, verbose, blockHash})
	if err != nil {
		return req, false
	}
	req.Params = params
	return req, true
}

// lookupActive is the index's block for txid, if that block is still in the
// node's active chain.
func (p *proxy) lookupActive(ctx context.Context, txid string) (string, error) {
	blockHash, err := p.ix.lookup(txid)
	if err != nil || blockHash == "" || p.inActiveChain == nil {
		return blockHash, err
	}
	active, err := p.inActiveChain(ctx, blockHash)
	if err != nil {
		return "", err
	}
	if !active {
		log.Printf("index has %s in block %s, which left the active chain", txid, blockHash)
		return "", nil
	}
	return blockHash, nil
}

// parseRequest reads one JSON-RPC request with the keys spelt exactly as the
// node reads them: Go's decoder would match "Method" or "METHOD" too, and
// the shim must not act on a method the node did not run.
func parseRequest(raw []byte) (rpcRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return rpcRequest{}, err
	}
	var req rpcRequest
	if err := json.Unmarshal(fields["method"], &req.Method); err != nil {
		return rpcRequest{}, errors.New("no method")
	}
	req.ID = fields["id"]
	req.Params = fields["params"]
	return req, nil
}

// retry resends a single request that the node answered with -5, with the
// block hash added, and returns the node's answer to that. ok is false when
// there is nothing better to answer with.
func (p *proxy) retry(r *http.Request, req rpcRequest) (json.RawMessage, bool) {
	rewritten, ok := p.withBlockHash(r.Context(), req)
	if !ok {
		return nil, false
	}
	body, err := json.Marshal(rewritten)
	if err != nil {
		return nil, false
	}
	_, out, err := p.forward(r, body)
	if err != nil {
		return nil, false
	}
	var resp rawResponse
	if json.Unmarshal(out, &resp) != nil || resp.Error != nil {
		return nil, false
	}
	return out, true
}

func isNotFound(resp rawResponse) bool {
	return resp.Error != nil && resp.Error.Code == rpcInvalidAddressOrKey
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "JSON-RPC over POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	status, out, err := p.forward(r, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		out = p.patchBatch(r, trimmed, out)
	case len(trimmed) > 0 && trimmed[0] == '{':
		var resp rawResponse
		if req, err := parseRequest(trimmed); err == nil && json.Unmarshal(out, &resp) == nil && isNotFound(resp) {
			if better, ok := p.retry(r, req); ok {
				out, status = better, http.StatusOK
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func (p *proxy) patchBatch(r *http.Request, body, out []byte) []byte {
	var rawReqs []json.RawMessage
	var resps []json.RawMessage
	if json.Unmarshal(body, &rawReqs) != nil || json.Unmarshal(out, &resps) != nil {
		return out
	}
	reqs := make([]rpcRequest, 0, len(rawReqs))
	for _, raw := range rawReqs {
		req, err := parseRequest(raw)
		if err != nil {
			return out
		}
		reqs = append(reqs, req)
	}

	// The node answers a batch in order, so answers pair with requests by
	// position. Otherwise by id, and only for ids that appear once: two
	// requests sharing an id cannot be told apart.
	inOrder := len(reqs) == len(resps)
	byID := make(map[string]rpcRequest, len(reqs))
	seen := make(map[string]int, len(reqs))
	for _, req := range reqs {
		byID[string(req.ID)] = req
		seen[string(req.ID)]++
	}

	changed := false
	for i, raw := range resps {
		var resp rawResponse
		if json.Unmarshal(raw, &resp) != nil || !isNotFound(resp) {
			continue
		}
		var req rpcRequest
		switch {
		case inOrder && bytes.Equal(reqs[i].ID, resp.ID):
			req = reqs[i]
		case seen[string(resp.ID)] == 1:
			req = byID[string(resp.ID)]
		default:
			continue
		}
		if better, ok := p.retry(r, req); ok {
			resps[i] = better
			changed = true
		}
	}
	if !changed {
		return out
	}
	patched, err := json.Marshal(resps)
	if err != nil {
		return out
	}
	return patched
}
