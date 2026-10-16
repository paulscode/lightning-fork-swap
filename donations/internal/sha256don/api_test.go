package sha256don

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/paulscode/lightning-fork-swap/donations/internal/channels"
)

type apiHarness struct {
	api   *API
	store *Memory
	mux   *http.ServeMux
	now   time.Time
}

func newAPI(t *testing.T) *apiHarness {
	h := &apiHarness{store: NewMemory(), mux: http.NewServeMux(),
		now: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)}
	h.store.Now = func() time.Time { return h.now }
	h.api = &API{Store: h.store, PoW: channels.NewPoW(2), Params: &chaincfg.MainNetParams,
		Now: func() time.Time { return h.now }, MaxPerDay: 3, Sha256Confs: 3, Blake2bConfs: 6,
		Verify: func(address, message, signature string) error {
			if signature != "signed:"+address+":"+message {
				return errors.New("bad proof")
			}
			return nil
		}}
	h.api.Register(h.mux)
	_ = h.store.SetSetting(context.Background(), SettingLastPass, h.now.Format(time.RFC3339))
	return h
}

func (h *apiHarness) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(method, path, &buf))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// work solves a fresh challenge
func (h *apiHarness) work() (string, string) {
	c := h.api.PoW.Challenge(h.now)
	for i := 0; ; i++ {
		n := strconv.Itoa(i)
		if channels.LeadingZeros(channels.Work(c, n)) >= h.api.PoW.Bits {
			return c, n
		}
	}
}

func (h *apiHarness) create(t *testing.T, ret string, replay bool) (int, map[string]any) {
	c, n := h.work()
	return h.do(t, "POST", "/donate/v1/sha256", map[string]any{
		"returnAddress": ret, "replay": replay, "challenge": c, "nonce": n})
}

func TestCreateAndRead(t *testing.T) {
	h := newAPI(t)
	code, info := h.do(t, "GET", "/donate/v1/sha256/info", nil)
	if code != 200 || info["available"] != true || info["challenge"] == "" {
		t.Fatalf("%d %v", code, info)
	}

	code, body := h.create(t, " "+dest+" ", true)
	if code != 201 || body["pending"] != true || body["returnAddress"] != dest || body["replay"] != true {
		t.Fatalf("%d %v", code, body)
	}
	id := body["id"].(string)
	// The worker gives the address
	_ = h.store.Assign(context.Background(), id, func(int64) (string, error) { return "bc1pours", nil })
	code, body = h.do(t, "GET", "/donate/v1/sha256/"+id, nil)
	if code != 200 || body["address"] != "bc1pours" || body["pending"] != false {
		t.Fatalf("%d %v", code, body)
	}
	if _, leaked := body["KeyIndex"]; leaked {
		t.Fatal("the key index is shown")
	}
	if code, _ := h.do(t, "GET", "/donate/v1/sha256/nosuchrequestxxxxxxxxx", nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
}

func TestCreateRefuses(t *testing.T) {
	h := newAPI(t)
	for name, c := range map[string]struct {
		ret    string
		replay bool
		code   string
	}{
		"a testnet address":         {"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", false, "bad_address"},
		"not an address":            {"hello", false, "bad_address"},
		"a copy without an address": {"", true, "replay_needs_address"},
	} {
		if code, body := h.create(t, c.ret, c.replay); code != 422 || body["code"] != c.code {
			t.Fatalf("%s: %d %v", name, code, body)
		}
	}
	// Without work
	if code, body := h.do(t, "POST", "/donate/v1/sha256", map[string]any{"challenge": "x", "nonce": "1"}); code != 400 || body["code"] != "work" {
		t.Fatalf("no work: %d %v", code, body)
	}
	// The day's budget
	for i := 0; i < 3; i++ {
		if code, _ := h.create(t, "", false); code != 201 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code, body := h.create(t, "", false); code != 503 || body["code"] != "budget" {
		t.Fatalf("over budget: %d %v", code, body)
	}
	// The worker stopped: paused
	h.now = h.now.Add(time.Hour)
	if code, body := h.create(t, "", false); code != 503 || body["code"] != "unavailable" {
		t.Fatalf("paused: %d %v", code, body)
	}
}

func TestHints(t *testing.T) {
	h := newAPI(t)
	_, body := h.create(t, dest, true)
	id := body["id"].(string)
	path := "/donate/v1/sha256/" + id + "/tx"
	msg := ProofMessage(id, txA, dest)

	code, _ := h.do(t, "POST", path, map[string]any{"txid": txA,
		"proofs": []Proof{{Address: "bc1qdonor", Signature: "signed:bc1qdonor:" + msg}}})
	if code != 202 {
		t.Fatalf("good proof: %d", code)
	}
	hints, _ := h.store.Hints(context.Background(), id, txA)
	if len(hints) != 1 || len(hints[0].Proofs) != 1 {
		t.Fatalf("%+v", hints)
	}
	if code, body := h.do(t, "POST", path, map[string]any{"txid": txA,
		"proofs": []Proof{{Address: "bc1qdonor", Signature: "forged"}}}); code != 422 || body["code"] != "bad_proof" {
		t.Fatalf("bad proof: %d %v", code, body)
	}
	if code, body := h.do(t, "POST", path, map[string]any{"txid": "nope"}); code != 422 || body["code"] != "bad_txid" {
		t.Fatalf("bad txid: %d %v", code, body)
	}
	// A txid alone is a hint too
	if code, _ := h.do(t, "POST", path, map[string]any{"txid": txB}); code != 202 {
		t.Fatalf("txid only: %d", code)
	}

	// Proofs for a request that didn't ask for a copy
	_, body = h.create(t, dest, false)
	other := "/donate/v1/sha256/" + body["id"].(string) + "/tx"
	if code, body := h.do(t, "POST", other, map[string]any{"txid": txA,
		"proofs": []Proof{{Address: "bc1qdonor", Signature: "x"}}}); code != 422 || body["code"] != "no_replay" {
		t.Fatalf("no replay: %d %v", code, body)
	}
}

func TestTheViewLinksDepositsToTheirForward(t *testing.T) {
	h := newAPI(t)
	ctx := context.Background()
	_, body := h.create(t, dest, false)
	id := body["id"].(string)
	d, _ := h.store.PutDeposit(ctx, Deposit{RequestID: id, Chain: BLAKE2b, Txid: txA,
		Vout: 1, ValueSat: 50_000, State: Seen})
	d.State = Ready
	_ = h.store.SaveDeposit(ctx, d)
	if _, err := h.store.CreateForward(ctx, Forward{RequestID: id, Txid: txB, Dest: dest,
		AmountSat: 49_000, FeeSat: 1000, State: Sent}, []int64{d.ID}); err != nil {
		t.Fatal(err)
	}
	_, body = h.do(t, "GET", "/donate/v1/sha256/"+id, nil)
	blake := body["blake2b"].([]any)[0].(map[string]any)
	fwd := body["forwards"].([]any)[0].(map[string]any)
	if blake["txid"] != txA || blake["forwardTxid"] != txB || fwd["txid"] != txB || fwd["to"] != dest {
		t.Fatalf("%v", body)
	}
}

func TestTheViewShowsTheLock(t *testing.T) {
	h := newAPI(t)
	ctx := context.Background()
	_, body := h.create(t, dest, false)
	id := body["id"].(string)
	d, _ := h.store.PutDeposit(ctx, Deposit{RequestID: id, Chain: SHA256, Txid: txA,
		Vout: 1, ValueSat: 50_000, Confirmations: 3, State: Seen})
	d.State = Confirmed
	_ = h.store.SaveDeposit(ctx, d)
	lid, _ := h.store.CreateLock(ctx, Lock{Txid: txB, KeyIndex: 0, ValueSat: 49_000, State: Sent}, []int64{d.ID})
	_ = h.store.EndLock(ctx, Lock{ID: lid, State: Done, Confirmations: 3})
	_, body = h.do(t, "GET", "/donate/v1/sha256/"+id, nil)
	sha := body["sha256"].([]any)[0].(map[string]any)
	if sha["state"] != Locked || sha["lockTxid"] != txB || sha["lockConfirmations"] != float64(3) {
		t.Fatalf("%v", sha)
	}
}

func TestOurOwnAddressesAreNoReturnAddress(t *testing.T) {
	h := newAPI(t)
	_, body := h.create(t, "", false)
	id := body["id"].(string)
	_ = h.store.Assign(context.Background(), id, func(int64) (string, error) {
		return "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr", nil
	})
	code, body := h.create(t, "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr", false)
	if code != 422 || body["code"] != "bad_address" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestHintsAreLimitedPerTransaction(t *testing.T) {
	h := newAPI(t)
	_, body := h.create(t, dest, true)
	path := "/donate/v1/sha256/" + body["id"].(string) + "/tx"
	for i := 0; i < MaxHintsPerTx; i++ {
		if code, _ := h.do(t, "POST", path, map[string]any{"txid": txA}); code != 202 {
			t.Fatalf("hint %d: %d", i, code)
		}
	}
	if code, body := h.do(t, "POST", path, map[string]any{"txid": txA}); code != 422 || body["code"] != "too_many_hints" {
		t.Fatalf("%d %v", code, body)
	}
	// Another transaction still has room
	if code, _ := h.do(t, "POST", path, map[string]any{"txid": txB}); code != 202 {
		t.Fatalf("other tx: %d", code)
	}
}
