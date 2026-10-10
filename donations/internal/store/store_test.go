package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
)

// Needs a Postgres: DONATIONS_TEST_DB=postgres://... go test ./internal/store
func TestPostgres(t *testing.T) {
	url := os.Getenv("DONATIONS_TEST_DB")
	if url == "" {
		t.Skip("DONATIONS_TEST_DB not set")
	}
	ctx := context.Background()
	db, err := Open(ctx, url, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool.Close()
	_, _ = db.Pool.Exec(ctx, "TRUNCATE replay_checks, worker_state")
	// The schema can be created again
	if _, err := Open(ctx, url, true); err != nil {
		t.Fatal(err)
	}

	txid := strings.Repeat("a", 64)
	if err := db.AddDonation(ctx, txid, "bc1pdonate", 1234); err != nil {
		t.Fatal(err)
	}
	if err := db.AddDonation(ctx, txid, "bc1pother", 1); err != nil {
		t.Fatal(err)
	}
	due, err := db.Due(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0].AmountSat != 1234 ||
		due[0].Verdict != replay.Pending || due[0].Address != "bc1pdonate" {
		t.Fatalf("due %+v %v", due, err)
	}

	checked := time.Now().UTC().Truncate(time.Microsecond)
	next := checked.Add(time.Hour)
	result := replay.Result{Verdict: replay.AtRisk, Reason: "r",
		Inputs:          []replay.Input{{Prevout: "x:1", Address: "bc1pdonor", PreFork: true}},
		AtRiskAddresses: []string{"bc1pdonor"}}
	if err := db.Save(ctx, txid, result, checked, next); err != nil {
		t.Fatal(err)
	}
	rec, err := db.Get(ctx, txid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != replay.AtRisk || rec.Inputs[0].Address != "bc1pdonor" ||
		!rec.Inputs[0].PreFork || rec.AtRiskAddresses[0] != "bc1pdonor" ||
		!rec.NextCheck.Equal(next) || rec.ReplayedHeight != nil {
		t.Fatalf("%+v", rec)
	}
	if due, _ := db.Due(ctx, checked.Add(time.Minute), 10); len(due) != 0 {
		t.Fatal("due before its next check")
	}

	final := replay.Result{Verdict: replay.Replayed, ReplayedHeight: 970000,
		AtRiskAddresses: []string{"bc1pdonor"}}
	if err := db.Save(ctx, txid, final, checked, time.Time{}); err != nil {
		t.Fatal(err)
	}
	rec, _ = db.Get(ctx, txid)
	if rec.NextCheck != nil || rec.ReplayedHeight == nil || *rec.ReplayedHeight != 970000 {
		t.Fatalf("%+v", rec)
	}
	if due, _ := db.Due(ctx, checked.Add(1000*time.Hour), 10); len(due) != 0 {
		t.Fatal("a final verdict is due again")
	}

	list, _ := db.List(ctx, []replay.Verdict{replay.Replayed})
	if len(list) != 1 {
		t.Fatalf("list %v", list)
	}
	if list, _ := db.List(ctx, []replay.Verdict{replay.AtRisk}); len(list) != 0 {
		t.Fatal("listed under another verdict")
	}
	if err := db.SetNote(ctx, txid, "returned in abc"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNote(ctx, strings.Repeat("b", 64), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("note on nothing: %v", err)
	}
	if _, err := db.Get(ctx, strings.Repeat("b", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get nothing: %v", err)
	}

	if v, _ := db.State(ctx, "k"); v != "" {
		t.Fatal("state before set")
	}
	_ = db.SetState(ctx, "k", "1")
	_ = db.SetState(ctx, "k", "2")
	if v, _ := db.State(ctx, "k"); v != "2" {
		t.Fatalf("state %q", v)
	}
}
