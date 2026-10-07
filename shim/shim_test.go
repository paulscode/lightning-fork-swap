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
	paths       []string
	prev        map[string]string // block hash -> previous block hash
	heights     map[string]int    // block hash -> height, stale blocks too
	// onRequest, when set, runs before each request is answered, with the
	// lock held: a test changes the chain mid-sync with it
	onRequest func(n *fakeNode, req rpcRequest)
}

func hash32(prefix string, n int) string {
	// Fixed width: "ba1" and "ba10" padded with zeros would be the same hash
	s := fmt.Sprintf("%s%08x", prefix, n)
	return s + strings.Repeat("0", 64-len(s))
}

func newFakeNode(blocks int) *fakeNode {
	n := &fakeNode{txs: map[string][]string{}, mempool: map[string]bool{}, prev: map[string]string{}, heights: map[string]int{}}
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
	if h > 0 {
		n.prev[bh] = n.chain[h-1]
	}
	n.heights[bh] = h
	n.chain = append(n.chain, bh)
	n.txs[bh] = []string{hash32("c"+fork, h), hash32("d"+fork, h)}
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.paths = append(n.paths, r.URL.RequestURI())
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
	if n.onRequest != nil {
		n.onRequest(n, req)
	}
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
	case "getbestblockhash":
		return ok(n.chain[len(n.chain)-1])
	case "getblockhash":
		h := int(params[0].(float64))
		if h >= len(n.chain) {
			return fail(-8, "Block height out of range")
		}
		return ok(n.chain[h])
	case "getblock":
		block := map[string]any{"tx": n.txs[params[0].(string)]}
		if prev, has := n.prev[params[0].(string)]; has {
			block["previousblockhash"] = prev
		}
		return ok(block)
	case "getblockheader":
		bh := params[0].(string)
		h, known := n.heights[bh]
		if !known {
			return fail(-5, "Block not found")
		}
		confirmations := -1
		if h < len(n.chain) && n.chain[h] == bh {
			confirmations = len(n.chain) - h
		}
		return ok(map[string]any{"hash": bh, "height": h, "confirmations": confirmations})
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

func TestLookupCatchesUpOnAMiss(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	// Wire the proxy to catch up, as main does.
	h.shim.Close()
	h.shim = httptest.NewServer(&proxy{upstream: h.nodeS.URL, ix: h.ix, http: &http.Client{Timeout: 10 * time.Second}, catchUp: h.idx.catchUp})

	// A block the poll loop has not seen yet.
	h.node.mu.Lock()
	h.node.addBlock("a", 30)
	h.node.mu.Unlock()

	status, out := h.post(t, `{"id":1,"method":"getrawtransaction","params":["`+hash32("ca", 30)+`"]}`)
	if status != http.StatusOK || !strings.Contains(out, "block:"+hash32("ba", 30)) {
		t.Fatalf("%d %s", status, out)
	}
}

func TestCatchUpIsOneCallWhenCurrent(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	before := len(h.node.calls)
	if err := h.idx.catchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.node.calls[before:]; len(got) != 1 || !strings.HasPrefix(got[0], "getbestblockhash") {
		t.Fatalf("calls when current: %v", got)
	}
}

// A reorganisation between the walk back and the indexing of new blocks
// must not leave the old block below the new ones.
func TestReorganisationDuringSync(t *testing.T) {
	h := newHarness(t, 50, 100)
	h.sync(t) // blocks 0 to 49 of the first chain
	h.node.mu.Lock()
	h.node.addBlock("a", 50)
	fired := false
	// After the walk back has found the index tip (49) current, and just
	// as block 50 is fetched, the node switches to a chain where 49 and 50
	// are other blocks
	h.node.onRequest = func(n *fakeNode, req rpcRequest) {
		if !fired && req.Method == "getblockhash" && string(req.Params) == "[50]" {
			fired = true
			n.addBlock("f", 49)
			n.addBlock("f", 50)
		}
	}
	h.node.mu.Unlock()

	h.sync(t)

	if !fired {
		t.Fatal("the reorganisation was not triggered")
	}
	if b, _ := h.ix.lookup(hash32("ca", 49)); b != "" {
		t.Errorf("the replaced block 49 is still indexed: %s", b)
	}
	if b, _ := h.ix.lookup(hash32("cf", 49)); b != hash32("bf", 49) {
		t.Errorf("the new block 49 is not indexed: %q", b)
	}
	if b, _ := h.ix.lookup(hash32("cf", 50)); b != hash32("bf", 50) {
		t.Errorf("the new block 50 is not indexed: %q", b)
	}
}

func TestProxyKeepsTheWalletPath(t *testing.T) {
	h := newHarness(t, 5, 10)
	req, _ := http.NewRequest(http.MethodPost, h.shim.URL+"/wallet/boltz?x=1", strings.NewReader(`{"id":1,"method":"getbalances","params":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.node.mu.Lock()
	defer h.node.mu.Unlock()
	if got := h.node.paths[len(h.node.paths)-1]; got != "/wallet/boltz?x=1" {
		t.Fatalf("the node was asked at %q", got)
	}
}

func TestProxyRetriesAtTheWalletPath(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	h.node.mu.Lock()
	h.node.paths = nil
	h.node.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, h.shim.URL+"/wallet/boltz", strings.NewReader(`{"id":1,"method":"getrawtransaction","params":["`+hash32("ca", 25)+`",true]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.node.mu.Lock()
	defer h.node.mu.Unlock()
	for _, p := range h.node.paths {
		if p != "/wallet/boltz" {
			t.Fatalf("a request went to %q", p)
		}
	}
}

func TestBatchWithDuplicateIDs(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	a, b := hash32("ca", 28), hash32("ca", 29)
	_, out := h.post(t, `[{"id":1,"method":"getrawtransaction","params":["`+a+`"]},{"id":1,"method":"getrawtransaction","params":["`+b+`"]}]`)
	var resps []rawResponse
	if err := json.Unmarshal([]byte(out), &resps); err != nil {
		t.Fatal(err)
	}
	if len(resps) != 2 {
		t.Fatalf("%s", out)
	}
	if got := string(resps[0].Result); !strings.Contains(got, a) {
		t.Errorf("first answer is %s, not for %s", got, a)
	}
	if got := string(resps[1].Result); !strings.Contains(got, b) {
		t.Errorf("second answer is %s, not for %s", got, b)
	}
}

// An index entry whose block left the active chain is not used, even
// before the index has caught up with the reorganisation.
func TestStaleIndexEntryIsNotUsed(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	h.shim.Close()
	h.shim = httptest.NewServer(&proxy{upstream: h.nodeS.URL, ix: h.ix, http: &http.Client{Timeout: 10 * time.Second}, inActiveChain: h.idx.inActiveChain})
	// Block 29 is replaced on the node; the index has not synced
	h.node.mu.Lock()
	h.node.addBlock("f", 29)
	h.node.mu.Unlock()

	_, out := h.post(t, `{"id":1,"method":"getrawtransaction","params":["`+hash32("ca", 29)+`"]}`)
	var resp rawResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if !isNotFound(resp) {
		t.Fatalf("answered %s for a transaction only in a stale block", out)
	}
	// Still found in a block that is active
	_, out = h.post(t, `{"id":1,"method":"getrawtransaction","params":["`+hash32("ca", 28)+`"]}`)
	if !strings.Contains(out, "block:"+hash32("ba", 28)) {
		t.Fatalf("answered %s", out)
	}
}

func TestMethodKeyMustBeExact(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	// The node runs "method"; a request whose "Method" is getrawtransaction
	// is not retried as one
	_, out := h.post(t, `{"id":1,"method":"nosuchmethod","Method":"getrawtransaction","params":["`+hash32("ca", 25)+`"]}`)
	if strings.Contains(out, "block:") {
		t.Fatalf("retried as another method: %s", out)
	}
	if _, err := parseRequest([]byte(`{"Method":"getrawtransaction"}`)); err == nil {
		t.Fatal("parsed a request without an exact method key")
	}
}

func TestRefusesAnOversizedBody(t *testing.T) {
	h := newHarness(t, 5, 10)
	resp, err := http.Post(h.shim.URL, "application/json", bytes.NewReader(make([]byte, maxBody+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestCatchUpDoesNotWaitForALongSync(t *testing.T) {
	h := newHarness(t, 30, 10)
	h.sync(t)
	h.node.mu.Lock()
	h.node.addBlock("a", 30)
	h.node.mu.Unlock()
	old := catchUpWait
	catchUpWait = 50 * time.Millisecond
	defer func() { catchUpWait = old }()

	h.idx.mu.Lock()
	start := time.Now()
	err := h.idx.catchUp(context.Background())
	h.idx.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s", waited)
	}
}

func TestSyncRecordsWhenItReachedTheTip(t *testing.T) {
	h := newHarness(t, 30, 10)
	if !h.idx.lastSynced.get().IsZero() {
		t.Fatal("synced before syncing")
	}
	h.sync(t)
	if time.Since(h.idx.lastSynced.get()) > time.Minute {
		t.Fatal("not recorded")
	}
}
