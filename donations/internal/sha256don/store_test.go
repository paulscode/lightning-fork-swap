package sha256don

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paulscode/lightning-fork-swap/donations/internal/channels"
)

// TestPostgres runs against a real Postgres when DONATIONS_TEST_DB is a
// superuser URL: a throwaway database owned by donations_worker, the
// schema created as that role, the API's role checked to insert only what
// it may, and the worker's steps run through.
func TestPostgres(t *testing.T) {
	admin := os.Getenv("DONATIONS_TEST_DB")
	if admin == "" {
		t.Skip("DONATIONS_TEST_DB not set")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// The roles are shared with other packages' tests, which go test runs
	// at the same time: one at a time
	lock, err := root.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(52436)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = lock.Exec(ctx, `SELECT pg_advisory_unlock(52436)`) }()
	db := fmt.Sprintf("sha256don_test_%d", time.Now().UnixNano())
	workerPw, apiPw := channels.NewID(), channels.NewID()
	for _, q := range []string{
		// Other packages' tests may create the roles at the same moment
		`DO $$ BEGIN CREATE ROLE donations_worker LOGIN;
           EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`,
		`DO $$ BEGIN CREATE ROLE donations_api LOGIN;
           EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`,
		fmt.Sprintf(`ALTER ROLE donations_worker PASSWORD '%s'`, workerPw),
		fmt.Sprintf(`ALTER ROLE donations_api PASSWORD '%s'`, apiPw),
		`CREATE DATABASE ` + db + ` OWNER donations_worker`,
	} {
		if _, err := root.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { _, _ = root.Exec(ctx, `DROP DATABASE `+db+` WITH (FORCE)`) }()
	connect := func(user, pw string) *pgxpool.Pool {
		u, _ := url.Parse(admin)
		u.User = url.UserPassword(user, pw)
		u.Path = "/" + db
		pool, err := pgxpool.New(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		return pool
	}
	workerPool := connect("donations_worker", workerPw)
	defer workerPool.Close()
	if _, err := workerPool.Exec(ctx, Schema); err != nil {
		t.Fatal(err)
	}
	// Twice: it must be idempotent, as the worker runs it at every start
	if _, err := workerPool.Exec(ctx, Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := workerPool.Exec(ctx, `GRANT SELECT ON ALL TABLES IN SCHEMA public TO donations_api`); err != nil {
		t.Fatal(err)
	}
	apiPool := connect("donations_api", apiPw)
	defer apiPool.Close()
	worker, api := &Postgres{Pool: workerPool}, &Postgres{Pool: apiPool}

	id, id2 := channels.NewID(), channels.NewID()
	if err := api.CreateRequest(ctx, Request{ID: id, ReturnAddress: dest, Replay: true}); err != nil {
		t.Fatalf("the API cannot create a request: %v", err)
	}
	if ok, err := api.CreateRequestWithin(ctx, Request{ID: id2}, time.Now().Add(-time.Hour), 5); err != nil || !ok {
		t.Fatalf("the API cannot create within the budget: %v %v", ok, err)
	}
	if ok, err := api.CreateRequestWithin(ctx, Request{ID: channels.NewID()}, time.Now().Add(-time.Hour), 2); err != nil || ok {
		t.Fatalf("created over the budget: %v %v", ok, err)
	}
	if err := api.AddHint(ctx, Hint{RequestID: id, Txid: txA, Proofs: []Proof{{Address: "a", Signature: "s"}}}); err != nil {
		t.Fatalf("the API cannot add a hint: %v", err)
	}
	for _, q := range []string{
		`UPDATE sha256_requests SET address = 'bc1pattacker'`,
		`UPDATE sha256_requests SET return_address = 'bc1qattacker'`,
		`INSERT INTO sha256_requests (id, address) VALUES ('x', 'bc1pattacker')`,
		`INSERT INTO sha256_requests (id, key_index) VALUES ('y', 7)`,
		`INSERT INTO sha256_deposits (request_id, chain, txid, vout, value_sat, state)
         VALUES ('` + id + `', 'blake2b', 'x', 0, 1, 'ready')`,
		`INSERT INTO sha256_forwards (txid, raw, dest, amount_sat, fee_sat, state)
         VALUES ('x', 'x', 'x', 1, 1, 'sent')`,
		`DELETE FROM sha256_requests`,
		`INSERT INTO sha256_settings VALUES ('last_pass', 'x')`,
	} {
		if _, err := apiPool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("the API could: %s (%v)", strings.Fields(q)[0]+" "+strings.Fields(q)[1], err)
		}
	}

	// Addresses: each its own key index, in order
	un, err := worker.Unassigned(ctx)
	if err != nil || len(un) != 2 {
		t.Fatalf("%+v %v", un, err)
	}
	for _, r := range un {
		if err := worker.Assign(ctx, r.ID, func(i int64) (string, error) {
			return fmt.Sprintf("bc1pours%d", i), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ours, err := api.IsRequestAddress(ctx, "bc1pours1"); err != nil || !ours {
		t.Fatalf("our address: %v %v", ours, err)
	}
	r, err := api.GetRequest(ctx, id)
	if err != nil || r.Address != "bc1pours0" || r.KeyIndex != 0 || !r.Replay || r.ReturnAddress != dest {
		t.Fatalf("%+v %v", r, err)
	}
	if r2, _ := api.GetRequest(ctx, id2); r2.KeyIndex != 1 {
		t.Fatalf("second index %d", r2.KeyIndex)
	}
	now := time.Now().UTC()
	if err := worker.Polled(ctx, id2, now); err != nil {
		t.Fatal(err)
	}
	as, _ := worker.Assigned(ctx)
	if len(as) != 2 || as[0].ID != id || as[1].LastPolled == nil {
		t.Fatalf("never-polled first: %+v", as)
	}
	if n, _ := api.CreatedSince(ctx, now.Add(-time.Hour)); n != 2 {
		t.Fatalf("created %d", n)
	}

	// Deposits: added once, updated after
	d, err := worker.PutDeposit(ctx, Deposit{RequestID: id, Chain: BLAKE2b, Txid: txA, Vout: 1,
		ValueSat: 50_000, Confirmations: 1, State: Seen})
	if err != nil || d.ID == 0 {
		t.Fatalf("%+v %v", d, err)
	}
	d.State, d.Missing = Ready, 2
	if err := worker.SaveDeposit(ctx, d); err != nil {
		t.Fatal(err)
	}
	d, _ = worker.PutDeposit(ctx, Deposit{RequestID: id, Chain: BLAKE2b, Txid: txA, Vout: 1,
		ValueSat: 50_000, Confirmations: 7, State: Seen})
	if d.State != Ready || d.Confirmations != 7 || d.Missing != 0 {
		t.Fatalf("an update reset the state: %+v", d)
	}

	// A forward takes ready deposits only, and once
	f := Forward{RequestID: id, Txid: txB, Raw: "00", Dest: dest, AmountSat: 49_000, FeeSat: 1000, State: Sent}
	fid, err := worker.CreateForward(ctx, f, []int64{d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.CreateForward(ctx, Forward{Txid: strings.Repeat("c", 64), Raw: "00",
		Dest: dest, State: Sent}, []int64{d.ID}); err == nil {
		t.Fatal("a deposit went into two forwards")
	}
	fwds, err := api.Forwards(ctx, id)
	if err != nil || len(fwds) != 1 || fwds[0].Txid != txB {
		t.Fatalf("%+v %v", fwds, err)
	}
	f.ID, f.State = fid, FailedSend
	if err := worker.EndForward(ctx, f); err != nil {
		t.Fatal(err)
	}
	if ds, _ := worker.DepositsIn(ctx, BLAKE2b, Ready); len(ds) != 1 {
		t.Fatalf("a failed forward kept its coins: %+v", ds)
	}
	fid, err = worker.CreateForward(ctx, Forward{RequestID: id, Txid: strings.Repeat("d", 64),
		Raw: "00", Dest: dest, State: Sent}, []int64{d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.EndForward(ctx, Forward{ID: fid, State: Done, Confirmations: 6}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := worker.DepositsIn(ctx, BLAKE2b, Forwarded); len(ds) != 1 {
		t.Fatalf("%+v", ds)
	}

	// Locks take confirmed SHA256 deposits, once; a failed one gives them back
	sd, err := worker.PutDeposit(ctx, Deposit{RequestID: id, Chain: SHA256, Txid: txA, Vout: 1,
		ValueSat: 60_000, Confirmations: 3, State: Seen})
	if err != nil {
		t.Fatal(err)
	}
	sd.State = Confirmed
	_ = worker.SaveDeposit(ctx, sd)
	if j, _ := worker.NextLockIndex(ctx); j != 0 {
		t.Fatalf("first lock index %d", j)
	}
	lid, err := worker.CreateLock(ctx, Lock{Txid: strings.Repeat("e", 64), Raw: "00",
		KeyIndex: 0, ValueSat: 59_000, FeeSat: 1000, State: Sent}, []int64{sd.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.CreateLock(ctx, Lock{Txid: strings.Repeat("f", 64), Raw: "00",
		KeyIndex: 1, State: Sent}, []int64{sd.ID}); err == nil {
		t.Fatal("a deposit went into two locks")
	}
	if err := worker.EndLock(ctx, Lock{ID: lid, State: FailedSend, Error: "refused"}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := worker.DepositsIn(ctx, SHA256, Confirmed); len(ds) != 1 || ds[0].LockID != 0 {
		t.Fatalf("a failed lock kept its coins: %+v", ds)
	}
	if j, _ := worker.NextLockIndex(ctx); j != 1 {
		t.Fatalf("a lock index was reused: %d", j)
	}
	lid, _ = worker.CreateLock(ctx, Lock{Txid: strings.Repeat("1", 64), Raw: "00",
		KeyIndex: 1, ValueSat: 59_000, State: Sent}, []int64{sd.ID})
	if err := worker.EndLock(ctx, Lock{ID: lid, State: Done, Confirmations: 3}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := worker.DepositsIn(ctx, SHA256, Locked); len(ds) != 1 || ds[0].LockID != lid {
		t.Fatalf("%+v", ds)
	}
	if ls, _ := api.Locks(ctx, id); len(ls) != 1 || ls[0].Txid != strings.Repeat("1", 64) {
		t.Fatalf("the request's locks: %+v", ls)
	}
	// The anchor chain: the confirmed lock's anchor is the next to spend,
	// until a lock (not failed) spends it
	if a, err := worker.Anchor(ctx); err != nil || a.ID != lid {
		t.Fatalf("anchor %+v %v", a, err)
	}
	if np, _ := worker.NotPermanent(ctx); len(np) != 1 || np[0].ID != lid {
		t.Fatalf("not permanent %+v", np)
	}
	next, err := worker.CreateLock(ctx, Lock{Txid: strings.Repeat("5", 64), Raw: "00", KeyIndex: 2,
		State: Sent, AnchorVout: 1, AnchorIn: lid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Anchor(ctx); err == nil {
		t.Fatal("an anchor spent by a lock in flight was offered")
	}
	_ = worker.EndLock(ctx, Lock{ID: next, State: FailedSend})
	if a, _ := worker.Anchor(ctx); a.ID != lid {
		t.Fatal("a failed lock kept the anchor")
	}
	if err := worker.SetPermanent(ctx, lid); err != nil {
		t.Fatal(err)
	}
	if np, _ := worker.NotPermanent(ctx); len(np) != 0 {
		t.Fatalf("still not permanent %+v", np)
	}
	if l, _ := api.GetLock(ctx, lid); !l.Permanent {
		t.Fatal("not marked")
	}

	// Sweeps take confirmed, unswept locks, once
	unswept, err := worker.Unswept(ctx)
	if err != nil || len(unswept) != 1 {
		t.Fatalf("%+v %v", unswept, err)
	}
	sid, err := worker.CreateSweep(ctx, Sweep{Txid: strings.Repeat("2", 64), Raw: "00",
		Dest: dest, AmountSat: 58_000, FeeSat: 1000, State: Sent}, []int64{lid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.CreateSweep(ctx, Sweep{Txid: strings.Repeat("3", 64), Raw: "00",
		Dest: dest, State: Sent}, []int64{lid}); err == nil {
		t.Fatal("a lock went into two sweeps")
	}
	if err := worker.EndSweep(ctx, Sweep{ID: sid, State: FailedSend}); err != nil {
		t.Fatal(err)
	}
	if un, _ := worker.Unswept(ctx); len(un) != 1 {
		t.Fatal("a failed sweep kept its lock")
	}
	sid, _ = worker.CreateSweep(ctx, Sweep{Txid: strings.Repeat("4", 64), Raw: "00",
		Dest: dest, State: Sent}, []int64{lid})
	if err := worker.EndSweep(ctx, Sweep{ID: sid, State: Done, Confirmations: 3}); err != nil {
		t.Fatal(err)
	}
	if un, _ := worker.Unswept(ctx); len(un) != 0 {
		t.Fatalf("swept, still unswept: %+v", un)
	}
	for _, q := range []string{`UPDATE sha256_sweeps SET dest = 'bc1qattacker'`,
		`UPDATE sha256_locks SET sweep_id = NULL`} {
		if _, err := apiPool.Exec(ctx, q); err == nil {
			t.Errorf("the API could: %s", q)
		}
	}
	// The hold reason and when it was looked at
	bd, _ := worker.DepositsIn(ctx, BLAKE2b, Forwarded)
	at := time.Now().UTC().Truncate(time.Microsecond)
	bd[0].Hold, bd[0].HoldChecked = "waiting", &at
	_ = worker.SaveDeposit(ctx, bd[0])
	if got, _ := worker.DepositsIn(ctx, BLAKE2b, Forwarded); got[0].Hold != "waiting" ||
		got[0].HoldChecked == nil || !got[0].HoldChecked.Equal(at) {
		t.Fatalf("%+v", got[0])
	}

	hints, err := worker.Hints(ctx, id, txA)
	if err != nil || len(hints) != 1 || hints[0].Proofs[0].Signature != "s" {
		t.Fatalf("%+v %v", hints, err)
	}
	if err := worker.SetSetting(ctx, SettingLastPass, "now"); err != nil {
		t.Fatal(err)
	}
	if v, _ := api.Setting(ctx, SettingLastPass); v != "now" {
		t.Fatalf("setting %q", v)
	}
}
