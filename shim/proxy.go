package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
)

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

func (p *proxy) forward(r *http.Request, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream, bytes.NewReader(body))
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

	blockHash, err := p.ix.lookup(txid)
	if err == nil && blockHash == "" && p.catchUp != nil {
		if err := p.catchUp(ctx); err != nil {
			log.Printf("catching up for %s: %v", txid, err)
		}
		blockHash, err = p.ix.lookup(txid)
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		var req rpcRequest
		var resp rawResponse
		if json.Unmarshal(trimmed, &req) == nil && json.Unmarshal(out, &resp) == nil && isNotFound(resp) {
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
	var reqs []rpcRequest
	var resps []json.RawMessage
	if json.Unmarshal(body, &reqs) != nil || json.Unmarshal(out, &resps) != nil {
		return out
	}

	byID := make(map[string]rpcRequest, len(reqs))
	for _, req := range reqs {
		byID[string(req.ID)] = req
	}

	changed := false
	for i, raw := range resps {
		var resp rawResponse
		if json.Unmarshal(raw, &resp) != nil || !isNotFound(resp) {
			continue
		}
		req, has := byID[string(resp.ID)]
		if !has {
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
