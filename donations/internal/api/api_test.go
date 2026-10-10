package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

func TestReplayVerdicts(t *testing.T) {
	mem := store.NewMemory()
	ctx := context.Background()
	atRisk := strings.Repeat("a", 64)
	safe := strings.Repeat("b", 64)
	_ = mem.AddDonation(ctx, atRisk, "bc1pdonate", 1000)
	_ = mem.AddDonation(ctx, safe, "bc1pdonate", 1000)
	_ = mem.Save(ctx, atRisk, replay.Result{Verdict: replay.AtRisk,
		AtRiskAddresses: []string{"bc1pdonor"},
		Inputs:          []replay.Input{{Prevout: "x:0", Address: "bc1pdonor"}}},
		time.Now(), time.Now())
	_ = mem.Save(ctx, safe, replay.Result{Verdict: replay.Protected,
		AtRiskAddresses: []string{"bc1pshouldnotshow"}}, time.Now(), time.Time{})
	h := Handler(mem)

	get := func(path string) (int, map[string]any, http.Header) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body, rec.Header()
	}

	code, body, headers := get("/donate/v1/replay/" + atRisk)
	if code != 200 || body["verdict"] != "at_risk" ||
		body["atRiskAddresses"].([]any)[0] != "bc1pdonor" {
		t.Fatalf("%d %v", code, body)
	}
	if _, ok := body["inputs"]; ok {
		t.Fatal("inputs are not for the browser")
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable")
	}
	code, body, _ = get("/donate/v1/replay/" + strings.ToUpper(safe))
	if code != 200 || body["verdict"] != "protected" ||
		len(body["atRiskAddresses"].([]any)) != 0 {
		t.Fatalf("%d %v", code, body)
	}
	if code, _, _ = get("/donate/v1/replay/" + strings.Repeat("c", 64)); code != 404 {
		t.Fatalf("unknown txid: %d", code)
	}
	if code, _, _ = get("/donate/v1/replay/..%2fx"); code != 400 {
		t.Fatalf("bad txid: %d", code)
	}
	if code, _, _ = get("/donate/v1/health"); code != 200 {
		t.Fatalf("health: %d", code)
	}
	if code, _, _ = get("/donate/v1/other"); code != 404 {
		t.Fatalf("other: %d", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/donate/v1/replay/"+atRisk, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}

	mem.Fail = errors.New("db down")
	if code, body, _ = get("/donate/v1/replay/" + atRisk); code != 503 ||
		strings.Contains(body["error"].(string), "db") {
		t.Fatalf("store down: %d %v", code, body)
	}
}
