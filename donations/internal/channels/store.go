package channels

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for an order that does not exist.
var ErrNotFound = errors.New("no such order")

// Store keeps orders. The API uses the first group, the worker the rest.
type Store interface {
	CreateOrder(ctx context.Context, o Order) error
	GetOrder(ctx context.Context, id string) (Order, error)
	Events(ctx context.Context, id string) ([]Event, error)
	// Orders not yet paid, and orders created in the last day
	Counts(ctx context.Context, now time.Time) (unpaid, today int, err error)
	// Orders for this node that took or may take a donated channel
	ForNode(ctx context.Context, pubkey string) (int, error)
	AddRequest(ctx context.Context, r Request) error

	Active(ctx context.Context) ([]Order, error)
	Save(ctx context.Context, o Order, events ...Event) error
	Requests(ctx context.Context) ([]Request, error)
	DoneRequest(ctx context.Context, id int64) error
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	Prune(ctx context.Context, before time.Time) error
}

// States that hold a node's donated channel, or may soon
var holdsChannel = []State{PaymentConfirmed, Connecting, Opening, FundingBroadcast,
	Open, Retrying, NeedsAttention}

// Schema is created by the worker (the database's owner). The API's role
// may read everything and insert only what a new order or an edit request
// carries; the worker fills in the rest.
const Schema = `
CREATE TABLE IF NOT EXISTS channel_orders (
    id text PRIMARY KEY,
    secret_hash text NOT NULL,
    node_pubkey text NOT NULL,
    node_addr text NOT NULL DEFAULT '',
    node_input text NOT NULL DEFAULT '',
    disclaimer_version text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    state text NOT NULL DEFAULT 'new',
    address text UNIQUE,
    node_alias text NOT NULL DEFAULT '',
    received_sat bigint NOT NULL DEFAULT 0,
    confirmed_sat bigint NOT NULL DEFAULT 0,
    utxos jsonb NOT NULL DEFAULT '[]',
    wumbo boolean NOT NULL DEFAULT false,
    channel_point text,
    capacity_sat bigint NOT NULL DEFAULT 0,
    fee_sat bigint NOT NULL DEFAULT 0,
    remainder_sat bigint NOT NULL DEFAULT 0,
    funding_confs bigint NOT NULL DEFAULT 0,
    attempts int NOT NULL DEFAULT 0,
    next_attempt timestamptz,
    error_code text NOT NULL DEFAULT '',
    attention_since timestamptz,
    failing_since timestamptz,
    opened_at timestamptz,
    closed_at timestamptz,
    version bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS channel_orders_state ON channel_orders (state);
CREATE INDEX IF NOT EXISTS channel_orders_node ON channel_orders (node_pubkey);
CREATE TABLE IF NOT EXISTS order_events (
    id bigserial PRIMARY KEY,
    order_id text NOT NULL REFERENCES channel_orders (id) ON DELETE CASCADE,
    at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL,
    detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS order_events_order ON order_events (order_id, id);
CREATE TABLE IF NOT EXISTS order_requests (
    id bigserial PRIMARY KEY,
    order_id text NOT NULL REFERENCES channel_orders (id) ON DELETE CASCADE,
    at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL,
    node_pubkey text NOT NULL,
    node_addr text NOT NULL DEFAULT '',
    node_input text NOT NULL DEFAULT '',
    done boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS channel_settings (key text PRIMARY KEY, value text NOT NULL);
DO $$ BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_api') THEN
    GRANT INSERT (id, secret_hash, node_pubkey, node_addr, node_input,
                  disclaimer_version, expires_at) ON channel_orders TO donations_api;
    GRANT INSERT (order_id, kind, node_pubkey, node_addr, node_input)
          ON order_requests TO donations_api;
    GRANT USAGE ON SEQUENCE order_requests_id_seq TO donations_api;
  END IF;
END $$;
`

// Postgres is the production store.
type Postgres struct{ Pool *pgxpool.Pool }

const orderColumns = `id, secret_hash, node_pubkey, node_addr, node_input, node_alias,
    state, coalesce(address, ''), disclaimer_version, created_at, expires_at, updated_at,
    received_sat, confirmed_sat, utxos, wumbo, coalesce(channel_point, ''), capacity_sat,
    fee_sat, remainder_sat, funding_confs, attempts, next_attempt, error_code,
    attention_since, failing_since, opened_at, closed_at, version`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	var utxos []byte
	err := row.Scan(&o.ID, &o.SecretHash, &o.NodePubkey, &o.NodeAddr, &o.NodeInput,
		&o.NodeAlias, &o.State, &o.Address, &o.DisclaimerVersion, &o.CreatedAt,
		&o.ExpiresAt, &o.UpdatedAt, &o.ReceivedSat, &o.ConfirmedSat, &utxos, &o.Wumbo,
		&o.ChannelPoint, &o.CapacitySat, &o.FeeSat, &o.RemainderSat, &o.FundingConfs,
		&o.Attempts, &o.NextAttempt, &o.ErrorCode, &o.AttentionSince, &o.FailingSince,
		&o.OpenedAt, &o.ClosedAt, &o.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(utxos, &o.Utxos)
	}
	return o, err
}

func (p *Postgres) CreateOrder(ctx context.Context, o Order) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO channel_orders (id, secret_hash, node_pubkey,
        node_addr, node_input, disclaimer_version, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7)`, o.ID, o.SecretHash, o.NodePubkey,
		o.NodeAddr, o.NodeInput, o.DisclaimerVersion, o.ExpiresAt)
	if err == nil {
		// Wakes the worker; it also looks every few seconds
		_, _ = p.Pool.Exec(ctx, `SELECT pg_notify('channel_orders', $1)`, o.ID)
	}
	return err
}

func (p *Postgres) GetOrder(ctx context.Context, id string) (Order, error) {
	return scanOrder(p.Pool.QueryRow(ctx, `SELECT `+orderColumns+
		` FROM channel_orders WHERE id = $1`, id))
}

func (p *Postgres) Events(ctx context.Context, id string) ([]Event, error) {
	rows, err := p.Pool.Query(ctx, `SELECT at, kind, detail FROM order_events
        WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var detail []byte
		if err := rows.Scan(&e.At, &e.Kind, &detail); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &e.Detail)
		if len(e.Detail) == 0 {
			e.Detail = nil
		}
		e.OrderID = id
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) Counts(ctx context.Context, now time.Time) (int, int, error) {
	var unpaid, today int
	err := p.Pool.QueryRow(ctx, `SELECT
        count(*) FILTER (WHERE state IN ('new', 'awaiting_payment') AND received_sat = 0),
        count(*) FILTER (WHERE created_at > $1)
        FROM channel_orders`, now.Add(-24*time.Hour)).Scan(&unpaid, &today)
	return unpaid, today, err
}

func (p *Postgres) ForNode(ctx context.Context, pubkey string) (int, error) {
	states := make([]string, len(holdsChannel))
	for i, s := range holdsChannel {
		states[i] = string(s)
	}
	var n int
	err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM channel_orders
        WHERE node_pubkey = $1 AND state = ANY($2)`, pubkey, states).Scan(&n)
	return n, err
}

func (p *Postgres) AddRequest(ctx context.Context, r Request) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO order_requests (order_id, kind, node_pubkey,
        node_addr, node_input) VALUES ($1, $2, $3, $4, $5)`,
		r.OrderID, r.Kind, r.Node, r.Addr, r.Input)
	if err == nil {
		_, _ = p.Pool.Exec(ctx, `SELECT pg_notify('channel_orders', $1)`, r.OrderID)
	}
	return err
}

func (p *Postgres) Active(ctx context.Context) ([]Order, error) {
	rows, err := p.Pool.Query(ctx, `SELECT `+orderColumns+` FROM channel_orders
        WHERE state NOT IN ('fell_back', 'closed', 'expired', 'rejected')
        ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (p *Postgres) Save(ctx context.Context, o Order, events ...Event) error {
	utxos, err := json.Marshal(o.Utxos)
	if err != nil {
		return err
	}
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var address any
	if o.Address != "" {
		address = o.Address
	}
	var point any
	if o.ChannelPoint != "" {
		point = o.ChannelPoint
	}
	if _, err := tx.Exec(ctx, `UPDATE channel_orders SET node_pubkey = $2, node_addr = $3,
        node_input = $4, node_alias = $5, state = $6, address = $7, expires_at = $8,
        received_sat = $9, confirmed_sat = $10, utxos = $11, wumbo = $12,
        channel_point = $13, capacity_sat = $14, fee_sat = $15, remainder_sat = $16,
        funding_confs = $17, attempts = $18, next_attempt = $19, error_code = $20,
        attention_since = $21, failing_since = $22, opened_at = $23, closed_at = $24,
        updated_at = now(), version = version + 1 WHERE id = $1`,
		o.ID, o.NodePubkey, o.NodeAddr, o.NodeInput, o.NodeAlias, string(o.State), address,
		o.ExpiresAt, o.ReceivedSat, o.ConfirmedSat, utxos, o.Wumbo, point, o.CapacitySat,
		o.FeeSat, o.RemainderSat, o.FundingConfs, o.Attempts, o.NextAttempt, o.ErrorCode,
		o.AttentionSince, o.FailingSince, o.OpenedAt, o.ClosedAt); err != nil {
		return err
	}
	for _, e := range events {
		detail, _ := json.Marshal(e.Detail)
		if e.Detail == nil {
			detail = []byte("{}")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, at, kind, detail)
            VALUES ($1, $2, $3, $4)`, o.ID, e.At, e.Kind, detail); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Requests(ctx context.Context) ([]Request, error) {
	rows, err := p.Pool.Query(ctx, `SELECT id, order_id, kind, node_pubkey, node_addr,
        node_input FROM order_requests WHERE NOT done ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		if err := rows.Scan(&r.ID, &r.OrderID, &r.Kind, &r.Node, &r.Addr, &r.Input); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) DoneRequest(ctx context.Context, id int64) error {
	_, err := p.Pool.Exec(ctx, `UPDATE order_requests SET done = true WHERE id = $1`, id)
	return err
}

func (p *Postgres) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := p.Pool.QueryRow(ctx, `SELECT value FROM channel_settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (p *Postgres) SetSetting(ctx context.Context, key, value string) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO channel_settings VALUES ($1, $2)
        ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Prune forgets final orders (and so their events and requests) last
// changed before the time, and done requests: no donor data is kept longer
// than it is useful.
func (p *Postgres) Prune(ctx context.Context, before time.Time) error {
	if _, err := p.Pool.Exec(ctx, `DELETE FROM channel_orders WHERE updated_at < $1
        AND state IN ('fell_back', 'closed', 'expired', 'rejected')`, before); err != nil {
		return err
	}
	_, err := p.Pool.Exec(ctx, `DELETE FROM order_requests WHERE done AND at < $1`, before)
	return err
}

// Memory is a Store for tests.
type Memory struct {
	mu       sync.Mutex
	Orders   map[string]Order
	Log      map[string][]Event
	Queue    []Request
	Settings map[string]string
	Fail     error
	now      func() time.Time
}

// NewMemory is an empty store; now stamps created and updated times.
func NewMemory(now func() time.Time) *Memory {
	return &Memory{Orders: map[string]Order{}, Log: map[string][]Event{},
		Settings: map[string]string{}, now: now}
}

func (m *Memory) CreateOrder(_ context.Context, o Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.Orders[o.ID]; ok {
		return errors.New("duplicate")
	}
	o.State, o.CreatedAt, o.UpdatedAt = New, m.now(), m.now()
	m.Orders[o.ID] = o
	return nil
}

func (m *Memory) GetOrder(_ context.Context, id string) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return Order{}, m.Fail
	}
	o, ok := m.Orders[id]
	if !ok {
		return o, ErrNotFound
	}
	return o, nil
}

func (m *Memory) Events(_ context.Context, id string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.Log[id]...), m.Fail
}

func (m *Memory) Counts(_ context.Context, now time.Time) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unpaid, today := 0, 0
	for _, o := range m.Orders {
		if (o.State == New || o.State == AwaitingPayment) && o.ReceivedSat == 0 {
			unpaid++
		}
		if o.CreatedAt.After(now.Add(-24 * time.Hour)) {
			today++
		}
	}
	return unpaid, today, m.Fail
}

func (m *Memory) ForNode(_ context.Context, pubkey string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, o := range m.Orders {
		for _, s := range holdsChannel {
			if o.NodePubkey == pubkey && o.State == s {
				n++
			}
		}
	}
	return n, m.Fail
}

func (m *Memory) AddRequest(_ context.Context, r Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	r.ID = int64(len(m.Queue) + 1)
	m.Queue = append(m.Queue, r)
	return nil
}

func (m *Memory) Active(context.Context) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Order
	for _, o := range m.Orders {
		if !o.State.Final() {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, m.Fail
}

func (m *Memory) Save(_ context.Context, o Order, events ...Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.Orders[o.ID]; !ok {
		return ErrNotFound
	}
	if o.Address != "" {
		for id, other := range m.Orders {
			if id != o.ID && other.Address == o.Address {
				return errors.New("address taken")
			}
		}
	}
	o.Version++
	o.UpdatedAt = m.now()
	m.Orders[o.ID] = o
	for _, e := range events {
		e.OrderID = o.ID
		m.Log[o.ID] = append(m.Log[o.ID], e)
	}
	return nil
}

func (m *Memory) Requests(context.Context) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Request
	for _, r := range m.Queue {
		if r.Kind != "" {
			out = append(out, r)
		}
	}
	return out, m.Fail
}

func (m *Memory) DoneRequest(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.Queue {
		if m.Queue[i].ID == id {
			m.Queue[i].Kind = ""
		}
	}
	return m.Fail
}

func (m *Memory) Setting(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Settings[key], m.Fail
}

func (m *Memory) SetSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Settings[key] = value
	return m.Fail
}

func (m *Memory) Prune(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, o := range m.Orders {
		if o.State.Final() && o.UpdatedAt.Before(before) {
			delete(m.Orders, id)
			delete(m.Log, id)
		}
	}
	return m.Fail
}
