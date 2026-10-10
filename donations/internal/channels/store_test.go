package channels

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgres runs against a real Postgres when DONATIONS_TEST_DB is a
// superuser URL: a throwaway database owned by donations_worker, the
// schema created as that role, and the API's role checked to insert only
// what it may.
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
	db := fmt.Sprintf("channels_test_%d", time.Now().UnixNano())
	workerPw, apiPw := NewID(), NewID()
	for _, q := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_worker')
           THEN CREATE ROLE donations_worker LOGIN; END IF;
           IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_api')
           THEN CREATE ROLE donations_api LOGIN; END IF; END $$`,
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
	// What init-donations-db.sh grants the API on the worker's tables
	if _, err := workerPool.Exec(ctx, `GRANT SELECT ON ALL TABLES IN SCHEMA public TO donations_api`); err != nil {
		t.Fatal(err)
	}
	apiPool := connect("donations_api", apiPw)
	defer apiPool.Close()
	worker, api := &Postgres{Pool: workerPool}, &Postgres{Pool: apiPool}

	now := time.Now().UTC().Truncate(time.Microsecond)
	id := NewID()
	_, hash := NewSecret()
	if err := api.CreateOrder(ctx, Order{ID: id, SecretHash: hash, NodePubkey: donorNode,
		NodeAddr: "91.190.100.60:9735", NodeInput: "91.190.100.60", DisclaimerVersion: "v1",
		ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatalf("the API cannot create an order: %v", err)
	}
	if err := api.AddRequest(ctx, Request{OrderID: id, Kind: "node", Node: donorNode}); err != nil {
		t.Fatalf("the API cannot ask for an edit: %v", err)
	}
	// What the API must not do
	for _, q := range []string{
		`UPDATE channel_orders SET state = 'open'`,
		`UPDATE channel_orders SET node_pubkey = 'x'`,
		`DELETE FROM channel_orders`,
		`INSERT INTO channel_orders (id, secret_hash, node_pubkey, disclaimer_version, expires_at, state)
         VALUES ('x', 'x', 'x', 'x', now(), 'open')`,
		`INSERT INTO channel_orders (id, secret_hash, node_pubkey, disclaimer_version, expires_at, address)
         VALUES ('y', 'x', 'x', 'x', now(), 'bc1qattacker')`,
		`INSERT INTO order_events (order_id, kind) VALUES ('` + id + `', 'open')`,
		`UPDATE order_requests SET done = true`,
		`INSERT INTO channel_settings VALUES ('min_sat', '1')`,
	} {
		if _, err := apiPool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("the API could: %s (%v)", strings.Fields(q)[0]+" "+strings.Fields(q)[1], err)
		}
	}

	o, err := worker.GetOrder(ctx, id)
	if err != nil || o.State != New || o.NodeAddr != "91.190.100.60:9735" {
		t.Fatalf("%+v %v", o, err)
	}
	o.State, o.Address = AwaitingPayment, "bc1pdonation1"
	o.Utxos = []Utxo{{Outpoint: strings.Repeat("a", 64) + ":1", AmountSat: 5, Confirmations: 2, Leased: true}}
	next := now.Add(time.Minute)
	o.NextAttempt, o.ReceivedSat = &next, 5
	if err := worker.Save(ctx, o, Event{At: now, Kind: "address", Detail: map[string]any{"address": "bc1pdonation1"}},
		Event{At: now, Kind: "payment_seen"}); err != nil {
		t.Fatal(err)
	}
	got, err := api.GetOrder(ctx, id)
	if err != nil || got.Version != 1 || got.Address != "bc1pdonation1" || len(got.Utxos) != 1 ||
		!got.Utxos[0].Leased || got.NextAttempt == nil || !got.NextAttempt.Equal(next) {
		t.Fatalf("%+v %v", got, err)
	}
	events, err := api.Events(ctx, id)
	if err != nil || len(events) != 2 || events[0].Detail["address"] != "bc1pdonation1" || events[1].Detail != nil {
		t.Fatalf("%+v %v", events, err)
	}
	if _, err := api.GetOrder(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	unpaid, today, err := api.Counts(ctx, time.Now())
	if err != nil || unpaid != 0 || today != 1 {
		t.Fatalf("counts %d %d %v", unpaid, today, err)
	}
	o.State = Open
	_ = worker.Save(ctx, o)
	if n, err := api.ForNode(ctx, donorNode); err != nil || n != 1 {
		t.Fatalf("for node %d %v", n, err)
	}
	active, err := worker.Active(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active %d %v", len(active), err)
	}
	reqs, err := worker.Requests(ctx)
	if err != nil || len(reqs) != 1 || reqs[0].Node != donorNode {
		t.Fatalf("%+v %v", reqs, err)
	}
	if err := worker.DoneRequest(ctx, reqs[0].ID); err != nil {
		t.Fatal(err)
	}
	if reqs, _ = worker.Requests(ctx); len(reqs) != 0 {
		t.Fatal("request not done")
	}
	if err := worker.SetSetting(ctx, SettingMinSat, "1000302"); err != nil {
		t.Fatal(err)
	}
	if v, err := api.Setting(ctx, SettingMinSat); err != nil || v != "1000302" {
		t.Fatalf("%q %v", v, err)
	}
	if v, _ := api.Setting(ctx, "missing"); v != "" {
		t.Fatal(v)
	}
	// Address unique
	other := NewID()
	_ = api.CreateOrder(ctx, Order{ID: other, SecretHash: hash, NodePubkey: donorNode,
		DisclaimerVersion: "v1", ExpiresAt: now})
	o2, _ := worker.GetOrder(ctx, other)
	o2.Address = "bc1pdonation1"
	if err := worker.Save(ctx, o2); err == nil {
		t.Fatal("an address used twice")
	}
	// Pruning: only final orders, changed before the time
	o.State = Closed
	_ = worker.Save(ctx, o)
	if err := worker.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.GetOrder(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("a final order kept")
	}
	if _, err := worker.GetOrder(ctx, other); err != nil {
		t.Fatal("an active order pruned")
	}
}
