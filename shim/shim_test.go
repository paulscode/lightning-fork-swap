package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNode is a pruned node without -txindex: getrawtransaction finds a
// confirmed transaction only when told its block.
type fakeNode struct {
	mu          sync.Mutex
	chain       []string            // block hash by height
	txs         map[string][]string // block hash -> txids
	mempool     map[string]bool
	pruneHeight int64
	calls       []string
}

func hash32(prefix string, n int) string {
	s := fmt.Sprintf("%s%x", prefix, n)
	return s + strings.Repeat("0", 64-len(s))
}

func newFakeNode(blocks int) *fakeNode {
	n := &fakeNode{txs: map[string][]string{}, mempool: map[string]bool{}}
	for h := 0; h < blocks; h++ {
		n.addBlock("a", h)
	}
	return n
}

func (n *fakeNode) addBlock(fork string, h int) {
	bh := hash32("b"+fork, h)
	if h < len(n.chain) {
		n.chain = n.chain[:h]
	}
	n.chain = append(n.chain, bh)
	n.txs[bh] = []string{hash32("c"+fork, h), hash32("d"+fork, h)}
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	n.mu.Lock()
	defer n.mu.Unlock()
	trimmed := bytes.TrimSpace(body)
	if trimmed[0] == '[' {
		var reqs []rpcRequest
		_ = json.Unmarshal(trimmed, &reqs)
		out := make([]any, 0, len(reqs))
		for _, req := range reqs {
			out = append(out, n.handle(req))
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var req rpcRequest
	_ = json.Unmarshal(trimmed, &req)
	resp := n.handle(req)
	if resp["error"] != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (n *fakeNode) handle(req rpcRequest) map[string]any {
	n.calls = append(n.calls, req.Method+" "+string(req.Params))
	ok := func(v any) map[string]any { return map[string]any{"result": v, "error": nil, "id": req.ID} }
	fail := func(code int, msg string) map[string]any {
		return map[string]any{"result": nil, "error": map[string]any{"code": code, "message": msg}, "id": req.ID}
	}
	var params []any
	if json.Unmarshal(req.Params, &params) != nil {
		var named map[string]any
		_ = json.Unmarshal(req.Params, &named)
		params = []any{named["txid"]}
	}
	switch req.Method {
	case "getblockchaininfo":
		return ok(map[string]any{"blocks": len(n.chain) - 1, "pruned": n.pruneHeight > 0, "pruneheight": n.pruneHeight})
	case "getblockhash":
		h := int(params[0].(float64))
		if h >= len(n.chain) {
			return fail(-8, "Block height out of range")
		}
		return ok(n.chain[h])
	case "getblock":
		return ok(map[string]any{"tx": n.txs[params[0].(string)]})
	case "getrawtransaction":
		txid := params[0].(string)
		if n.mempool[txid] {
			return ok("mempool:" + txid)
		}
		if len(params) == 3 {
			for _, id := range n.txs[params[2].(string)] {
				if id == txid {
					return ok("block:" + params[2].(string) + ":" + txid)
				}
			}
			return fail(-5, "No such transaction found in the provided block")
		}
		return fail(-5, "No such mempool transaction. Use -txindex or provide a block hash")
	}
	return fail(-32601, "Method not found")
}

type harness struct {
	node  *fakeNode
	ix    *index
	idx   *indexer
	shim  *httptest.Server
	nodeS *httptest.Server
}

func newHarness(t *testing.T, blocks int, window int64) *harness {
	t.Helper()
	node := newFakeNode(blocks)
	nodeS := httptest.NewServer(node)
	ix, err := openIndex(filepath.Join(t.TempDir(), "ix.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		node: node, ix: ix, nodeS: nodeS,
		idx: &indexer{rpc: newRPCClient(nodeS.URL, "u", "p"), ix: ix, window: window},
	}
	h.shim = httptest.NewServer(&proxy{upstream: nodeS.URL, ix: ix, http: &http.Client{Timeout: 10 * time.Second}})
	t.Cleanup(func() { h.shim.Close(); nodeS.Close(); ix.close() })
	return h
}

func (h *harness) sync(t *testing.T) {
	t.Helper()
	if err := h.idx.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) post(t *testing.T, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(h.shim.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestIndexesTheWindow(t *testing.T) {
	h := newHarness(t, 100, 10)
	h.sync(t)
	if tip, _ := h.ix.tip(); tip != 99 {
		t.Fatalf("tip %d", tip)
	}
	if n, _ := h.ix.count(); n != 20 {
		t.Fatalf("indexed %d transactions, want 20 (10 blocks of 2)", n)
	}
	if b, _ := h.ix.lookup(hash32("ca", 95)); b != hash32("ba", 95) {
		t.Fatalf("lookup gave %q", b)
	}
	if b, _ := h.ix.lookup(hash32("ca", 85)); b != "" {
		t.Fatalf("a block outside the window is indexed: %q", b)
	}

	// The window moves on.
	h.node.addBlock("a", 100)
	h.node.addBlock("a", 101)
	h.sync(t)
	if b, _ := h.ix.lookup(hash32("ca", 91)); b != "" {
		t.Fatal("block 91 should have left the window")
	}
	if b, _ := h.ix.lookup(hash32("da", 101)); b != hash32("ba", 101) {
		t.Fatal("block 101 not indexed")
	}
}

func TestRespectsPruneHeight(t *testing.T) {
	h := newHarness(t, 100, 50)
	h.node.pruneHeight = 80
	h.sync(t)
	if b, _ := h.ix.lookup(hash32("ca", 79)); b != "" {
		t.Fatal("indexed a pruned block")
	}
	if b, _ := h.ix.lookup(hash32("ca", 80)); b == "" {
		t.Fatal("did not index from the prune height")
	}
}

func TestFollowsAReorganisation(t *testing.T) {
	h := newHarness(t, 50, 20)
	h.sync(t)

	// Blocks 45.. are replaced by another branch, one block longer.
	for height := 45; height <= 50; height++ {
		h.node.addBlock("f", height)
	}
	h.sync(t)

	if b, _ := h.ix.lookup(hash32("ca", 46)); b != "" {
		t.Fatal("a transaction of the abandoned branch is still indexed")
	}
	if b, _ := h.ix.lookup(hash32("cf", 46)); b != hash32("bf", 46) {
		t.Fatal("the new branch is not indexed")
	}
	if b, _ := h.ix.lookup(hash32("ca", 44)); b != hash32("ba", 44) {
		t.Fatal("a block below the fork was dropped")
	}

	// And a reorganisation to a shorter chain.
	h.node.chain = h.node.chain[:48]
	h.sync(t)
	if tip, _ := h.ix.tip(); tip != 47 {
		t.Fatalf("tip %d after the chain shrank to 47", tip)
	}
}

func TestProxyAddsTheBlockHash(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	txid := hash32("ca", 25)

	status, out := h.post(t, `{"jsonrpc":"1.0","id":7,"method":"getrawtransaction","params":["`+txid+`",true]}`)
	if status != http.StatusOK || !strings.Contains(out, "block:"+hash32("ba", 25)) || !strings.Contains(out, `"id":7`) {
		t.Fatalf("%d %s", status, out)
	}
	last := h.node.calls[len(h.node.calls)-1]
	if !strings.Contains(last, `true,"`+hash32("ba", 25)) {
		t.Fatalf("verbose flag not kept: %s", last)
	}

	// Named parameters.
	status, out = h.post(t, `{"id":"x","method":"getrawtransaction","params":{"txid":"`+txid+`"}}`)
	if status != http.StatusOK || !strings.Contains(out, "block:") {
		t.Fatalf("named: %d %s", status, out)
	}

	// A mempool transaction is answered by the node directly.
	h.node.mempool[hash32("e", 1)] = true
	_, out = h.post(t, `{"id":1,"method":"getrawtransaction","params":["`+hash32("e", 1)+`"]}`)
	if !strings.Contains(out, "mempool:") {
		t.Fatalf("mempool: %s", out)
	}

	// Unknown to both: the node's error, with its status, is passed on.
	status, out = h.post(t, `{"id":1,"method":"getrawtransaction","params":["`+hash32("f", 1)+`"]}`)
	if status != http.StatusInternalServerError || !strings.Contains(out, `"code":-5`) {
		t.Fatalf("unknown: %d %s", status, out)
	}

	// A request that already names a block is left alone.
	_, out = h.post(t, `{"id":1,"method":"getrawtransaction","params":["`+txid+`",false,"`+hash32("ba", 24)+`"]}`)
	if !strings.Contains(out, `"code":-5`) {
		t.Fatalf("explicit block hash was rewritten: %s", out)
	}

	// Other methods pass through.
	_, out = h.post(t, `{"id":1,"method":"getblockhash","params":[3]}`)
	if !strings.Contains(out, hash32("ba", 3)) {
		t.Fatalf("passthrough: %s", out)
	}
}

func TestProxyPatchesBatches(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	h.node.mempool[hash32("e", 1)] = true

	status, out := h.post(t, `[
		{"id":1,"method":"getrawtransaction","params":["`+hash32("ca", 21)+`"]},
		{"id":2,"method":"getrawtransaction","params":["`+hash32("e", 1)+`"]},
		{"id":3,"method":"getrawtransaction","params":["`+hash32("f", 9)+`"]},
		{"id":4,"method":"getrawtransaction","params":["`+hash32("da", 29)+`",1]}
	]`)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	var resps []rawResponse
	if err := json.Unmarshal([]byte(out), &resps); err != nil {
		t.Fatal(err)
	}
	if len(resps) != 4 {
		t.Fatalf("%d responses", len(resps))
	}
	byID := map[string]rawResponse{}
	for _, r := range resps {
		byID[string(r.ID)] = r
	}
	if !strings.Contains(string(byID["1"].Result), "block:"+hash32("ba", 21)) {
		t.Fatalf("1: %s", byID["1"].Result)
	}
	if !strings.Contains(string(byID["2"].Result), "mempool:") {
		t.Fatalf("2: %s", byID["2"].Result)
	}
	if byID["3"].Error == nil || byID["3"].Error.Code != -5 {
		t.Fatalf("3 should still be not found")
	}
	if !strings.Contains(string(byID["4"].Result), "block:"+hash32("ba", 29)) {
		t.Fatalf("4: %s", byID["4"].Result)
	}
}

func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	node := newFakeNode(40)
	nodeS := httptest.NewServer(node)
	defer nodeS.Close()

	for i := 0; i < 2; i++ {
		ix, err := openIndex(filepath.Join(dir, "ix.db"))
		if err != nil {
			t.Fatal(err)
		}
		idx := &indexer{rpc: newRPCClient(nodeS.URL, "u", "p"), ix: ix, window: 10}
		before := len(node.calls)
		if err := idx.sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		getblocks := 0
		for _, c := range node.calls[before:] {
			if strings.HasPrefix(c, "getblock ") {
				getblocks++
			}
		}
		want := 10
		if i == 1 {
			want = 0 // nothing new since the first run
		}
		if getblocks != want {
			t.Fatalf("run %d fetched %d blocks, want %d", i, getblocks, want)
		}
		ix.close()
	}
}

func TestForwardRelaysBothWays(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = forward(ctx, listen, echo.Addr().String()) }()

	var conn net.Conn
	for i := 0; i < 50; i++ {
		if conn, err = net.Dial("tcp", listen); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := []byte("rawblock payload")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("got %q", buf)
	}
}

func TestParseForwards(t *testing.T) {
	f, err := parseForwards("28332=knots:28332, 28333=knots:28333")
	if err != nil || f[":28332"] != "knots:28332" || f[":28333"] != "knots:28333" {
		t.Fatalf("%v %v", f, err)
	}
	if _, err := parseForwards("nonsense"); err == nil {
		t.Fatal("accepted a bad spec")
	}
}
