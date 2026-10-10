package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

type resolver map[string]string

func (r resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return []netip.Addr{netip.MustParseAddr(a)}, nil
	}
	return nil, &net404{}
}

type net404 struct{}

func (*net404) Error() string { return "no such host" }

func solve(t *testing.T, p *PoW, challenge string) string {
	t.Helper()
	for i := 0; i < 1<<22; i++ {
		nonce := strconv.Itoa(i)
		if LeadingZeros(Work(challenge, nonce)) >= p.Bits {
			return nonce
		}
	}
	t.Fatal("no nonce")
	return ""
}

type apiHarness struct {
	t     *testing.T
	api   *API
	mux   *http.ServeMux
	store *Memory
	now   time.Time
}

func newAPI(t *testing.T) *apiHarness {
	h := &apiHarness{t: t, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	h.store = NewMemory(func() time.Time { return h.now })
	h.api = &API{Store: h.store, Rules: DefaultRules(), PoW: NewPoW(8),
		Resolver: resolver{"node.example.com": "91.190.100.60", "inside.example": "10.0.0.2"},
		Now:      func() time.Time { return h.now }}
	h.mux = http.NewServeMux()
	h.api.Register(h.mux)
	h.alive()
	return h
}

// The worker passed just now
func (h *apiHarness) alive() {
	h.store.Settings[SettingLastPass] = h.now.Format(time.RFC3339)
	h.store.Settings[SettingFeeRate] = "3"
	h.store.Settings[SettingMinSat] = "1000906"
	h.store.Settings[SettingOurNode] = ourNode
}

func (h *apiHarness) do(method, path string, body any, headers ...string) (int, map[string]any, http.Header) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

func (h *apiHarness) createBody(node string) map[string]any {
	_, info, _ := h.do("GET", "/donate/v1/info", nil)
	challenge := info["challenge"].(string)
	return map[string]any{"node": node, "accepted": true,
		"disclaimerVersion": h.api.Rules.DisclaimerVersion, "challenge": challenge,
		"nonce": solve(h.t, h.api.PoW, challenge)}
}

func TestInfo(t *testing.T) {
	h := newAPI(t)
	code, info, _ := h.do("GET", "/donate/v1/info", nil)
	if code != 200 || info["available"] != true || info["minSat"] != float64(1000906) ||
		info["maxSat"] != float64(16_777_215) || info["openSlots"] != float64(100) ||
		info["powBits"] != float64(8) {
		t.Fatalf("%d %v", code, info)
	}
	h.now = h.now.Add(6 * time.Minute) // the worker stopped
	if _, info, _ = h.do("GET", "/donate/v1/info", nil); info["available"] != false {
		t.Fatal("available without a worker")
	}
}

func TestCreateAnOrder(t *testing.T) {
	h := newAPI(t)
	h.api.AddressWait = 2 * time.Second
	// The worker hands out the address meanwhile
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(20 * time.Millisecond)
			orders, _ := h.store.Active(context.Background())
			for _, o := range orders {
				if o.State == New {
					o.State, o.Address = AwaitingPayment, "bc1pdonation1"
					_ = h.store.Save(context.Background(), o)
					return
				}
			}
		}
	}()
	code, out, _ := h.do("POST", "/donate/v1/channel-orders", h.createBody(donorNode+"@node.example.com"))
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	id, secret := out["id"].(string), out["secret"].(string)
	order := out["order"].(map[string]any)
	if !ValidID(id) || len(secret) < 40 || order["address"] != "bc1pdonation1" ||
		order["state"] != "awaiting_payment" {
		t.Fatalf("%v", out)
	}
	stored := h.store.Orders[id]
	if stored.NodeAddr != "91.190.100.60:9735" || stored.NodeInput != "node.example.com" ||
		!SecretMatches(secret, stored.SecretHash) || stored.SecretHash == secret {
		t.Fatalf("%+v", stored)
	}
	if !stored.ExpiresAt.Equal(h.now.Add(24 * time.Hour)) {
		t.Fatalf("expires %v", stored.ExpiresAt)
	}
	// The secret is never shown again
	_, view, _ := h.do("GET", "/donate/v1/channel-orders/"+id, nil)
	if strings.Contains(mustJSON(view), secret) || strings.Contains(mustJSON(view), stored.SecretHash) {
		t.Fatal("the secret is in the view")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCreateRefusals(t *testing.T) {
	h := newAPI(t)
	cases := []struct {
		name   string
		body   func() map[string]any
		status int
		code   string
	}{
		{"terms not accepted", func() map[string]any {
			b := h.createBody(donorNode)
			b["accepted"] = false
			return b
		}, 409, "disclaimer"},
		{"terms of another version", func() map[string]any {
			b := h.createBody(donorNode)
			b["disclaimerVersion"] = "old"
			return b
		}, 409, "disclaimer"},
		{"no work", func() map[string]any {
			b := h.createBody(donorNode)
			b["nonce"] = "x"
			for LeadingZeros(Work(b["challenge"].(string), b["nonce"].(string))) >= 8 {
				b["nonce"] = b["nonce"].(string) + "x"
			}
			return b
		}, 400, "work"},
		{"a made-up challenge", func() map[string]any {
			b := h.createBody(donorNode)
			b["challenge"] = strings.Repeat("A", 43)
			return b
		}, 400, "work"},
		{"not a key", func() map[string]any { return h.createBody("02abc") }, 422, "bad_node"},
		{"not on the curve", func() map[string]any { return h.createBody("02" + strings.Repeat("ff", 32)) }, 422, "bad_node"},
		{"a private host", func() map[string]any { return h.createBody(donorNode + "@inside.example") }, 422, "not_public"},
		{"a loopback address", func() map[string]any { return h.createBody(donorNode + "@127.0.0.1:5432") }, 422, "not_public"},
		{"a name that does not resolve", func() map[string]any { return h.createBody(donorNode + "@nowhere.example") }, 422, "bad_host"},
		{"our own node", func() map[string]any { return h.createBody(ourNode) }, 422, ErrOurNode},
		{"an unknown field", func() map[string]any {
			b := h.createBody(donorNode)
			b["pushSat"] = 1000
			return b
		}, 400, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, _ := h.do("POST", "/donate/v1/channel-orders", c.body())
			if code != c.status || out["code"] != c.code {
				t.Fatalf("%d %v", code, out)
			}
		})
	}
	if len(h.store.Orders) != 0 {
		t.Fatal("an order was created")
	}
	// A challenge serves once
	body := h.createBody(donorNode)
	if code, _, _ := h.do("POST", "/donate/v1/channel-orders", body); code != 201 {
		t.Fatal(code)
	}
	if code, out, _ := h.do("POST", "/donate/v1/channel-orders", body); code != 400 || out["code"] != "work" {
		t.Fatalf("a challenge used twice: %d", code)
	}
}

func TestCreateWhenFullOrStopped(t *testing.T) {
	h := newAPI(t)
	h.api.Rules.MaxUnpaid = 1
	if code, _, _ := h.do("POST", "/donate/v1/channel-orders", h.createBody(donorNode)); code != 201 {
		t.Fatal(code)
	}
	if code, out, _ := h.do("POST", "/donate/v1/channel-orders", h.createBody(donorNode)); code != 503 || out["code"] != ErrBudget {
		t.Fatalf("%d %v", code, out)
	}
	// A node with its two donated channels
	h = newAPI(t)
	for i := 0; i < 2; i++ {
		id := NewID()
		_ = h.store.CreateOrder(context.Background(), Order{ID: id, NodePubkey: donorNode})
		o := h.store.Orders[id]
		o.State = Open
		h.store.Orders[id] = o
	}
	if code, out, _ := h.do("POST", "/donate/v1/channel-orders", h.createBody(donorNode)); code != 422 || out["code"] != ErrNodeLimit {
		t.Fatalf("%d %v", code, out)
	}
	h = newAPI(t)
	h.now = h.now.Add(10 * time.Minute)
	if code, out, _ := h.do("POST", "/donate/v1/channel-orders", h.createBody(donorNode)); code != 503 || out["code"] != "unavailable" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestTheOrderPage(t *testing.T) {
	h := newAPI(t)
	id := NewID()
	_ = h.store.CreateOrder(context.Background(), Order{ID: id, NodePubkey: donorNode,
		NodeInput: "node.example.com", DisclaimerVersion: "v"})
	o := h.store.Orders[id]
	o.State, o.Address, o.ReceivedSat = PaymentSeen, "bc1pdonation1", 500_000
	_ = h.store.Save(context.Background(), o,
		Event{At: h.now, Kind: "connecting", Detail: map[string]any{"node": "91.190.100.60:9735", "secretThing": 1}},
		Event{At: h.now, Kind: "retry", Detail: map[string]any{"code": ErrUnreachable}})
	code, view, headers := h.do("GET", "/donate/v1/channel-orders/"+id, nil)
	if code != 200 || view["state"] != "payment_seen" || view["receivedSat"] != float64(500_000) ||
		view["editable"] != true || view["confirmationsNeeded"] != float64(3) {
		t.Fatalf("%d %v", code, view)
	}
	timeline := view["timeline"].([]any)
	first := timeline[0].(map[string]any)
	if first["kind"] != "connecting" || first["detail"] != nil {
		t.Fatalf("detail not filtered: %v", first)
	}
	if timeline[1].(map[string]any)["detail"].(map[string]any)["code"] != ErrUnreachable {
		t.Fatal("public detail dropped")
	}
	etag := headers.Get("ETag")
	if code, _, _ := h.do("GET", "/donate/v1/channel-orders/"+id, nil, "If-None-Match", etag); code != 304 {
		t.Fatalf("no 304: %d", code)
	}
	_ = h.store.Save(context.Background(), h.store.Orders[id])
	if code, _, _ := h.do("GET", "/donate/v1/channel-orders/"+id, nil, "If-None-Match", etag); code != 200 {
		t.Fatal("a stale ETag matched")
	}
	for _, bad := range []string{NewID(), "short", "a%2Fb"} {
		if code, _, _ := h.do("GET", "/donate/v1/channel-orders/"+bad, nil); code != 404 {
			t.Errorf("%s: %d", bad, code)
		}
	}
}

func TestEditingTheNode(t *testing.T) {
	h := newAPI(t)
	id := NewID()
	secret, hash := NewSecret()
	_ = h.store.CreateOrder(context.Background(), Order{ID: id, SecretHash: hash, NodePubkey: donorNode})
	path := "/donate/v1/channel-orders/" + id + "/node"
	if code, _, _ := h.do("PATCH", path, map[string]any{"secret": "wrong", "node": donorNode}); code != 403 {
		t.Fatalf("wrong secret: %d", code)
	}
	if code, _, _ := h.do("PATCH", "/donate/v1/channel-orders/"+NewID()+"/node",
		map[string]any{"secret": secret, "node": donorNode}); code != 403 {
		t.Fatalf("another order: %d", code)
	}
	if code, out, _ := h.do("PATCH", path, map[string]any{"secret": secret, "node": donorNode + "@10.0.0.1"}); code != 422 || out["code"] != "not_public" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out, _ := h.do("PATCH", path, map[string]any{"secret": secret, "node": ourNode}); code != 422 || out["code"] != ErrOurNode {
		t.Fatalf("%d %v", code, out)
	}
	if code, _, _ := h.do("PATCH", path, map[string]any{"secret": secret, "node": donorNode + "@91.190.100.60:9736"}); code != 202 {
		t.Fatal(code)
	}
	if len(h.store.Queue) != 1 || h.store.Queue[0].Addr != "91.190.100.60:9736" {
		t.Fatalf("%+v", h.store.Queue)
	}
	o := h.store.Orders[id]
	o.State = FundingBroadcast
	h.store.Orders[id] = o
	if code, out, _ := h.do("PATCH", path, map[string]any{"secret": secret, "node": donorNode}); code != 409 || out["code"] != "not_editable" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestPoW(t *testing.T) {
	p := NewPoW(10)
	now := time.Now()
	c := p.Challenge(now)
	nonce := solve(t, p, c)
	if err := p.Verify(c, nonce, now.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(c, nonce, now); err != ErrUsed {
		t.Fatalf("reused: %v", err)
	}
	c = p.Challenge(now)
	nonce = solve(t, p, c)
	if err := p.Verify(c, nonce, now.Add(11*time.Minute)); err != ErrChallenge {
		t.Fatalf("expired: %v", err)
	}
	other := NewPoW(10)
	c = other.Challenge(now)
	if err := p.Verify(c, solve(t, other, c), now); err != ErrChallenge {
		t.Fatalf("another key's challenge: %v", err)
	}
	if LeadingZeros([32]byte{0, 0x0f}) != 12 || LeadingZeros([32]byte{0x80}) != 0 {
		t.Fatal("leading zeros")
	}
}
