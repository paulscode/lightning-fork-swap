package sha256don

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound: no such request.
var ErrNotFound = errors.New("no such request")

// Store keeps requests, deposits and forwards. The API uses the first
// group, the worker the rest.
type Store interface {
	CreateRequest(ctx context.Context, r Request) error
	// Creates the request only if fewer than max were created since then,
	// counted and inserted at once
	CreateRequestWithin(ctx context.Context, r Request, since time.Time, max int) (bool, error)
	// One of our requests' addresses
	IsRequestAddress(ctx context.Context, address string) (bool, error)
	GetRequest(ctx context.Context, id string) (Request, error)
	Deposits(ctx context.Context, requestID string) ([]Deposit, error)
	Forwards(ctx context.Context, requestID string) ([]Forward, error)
	CreatedSince(ctx context.Context, since time.Time) (int, error)
	AddHint(ctx context.Context, h Hint) error
	Setting(ctx context.Context, key string) (string, error)

	// Requests without an address yet
	Unassigned(ctx context.Context) ([]Request, error)
	// Gives a request the next key index and its address
	Assign(ctx context.Context, id string, address func(index int64) (string, error)) error
	// Requests with an address, the least recently looked at first
	Assigned(ctx context.Context) ([]Request, error)
	// Requests due a look (PollEvery by age, or a hint since the last
	// look), the least recently looked at first, at most limit
	Due(ctx context.Context, now time.Time, limit int) ([]Request, error)
	Polled(ctx context.Context, id string, at time.Time) error
	// Adds a deposit, or updates its value and confirmations
	PutDeposit(ctx context.Context, d Deposit) (Deposit, error)
	SaveDeposit(ctx context.Context, d Deposit) error
	DepositsIn(ctx context.Context, chain string, states ...string) ([]Deposit, error)
	Hints(ctx context.Context, requestID, txid string) ([]Hint, error)
	// Records a forward and marks its deposits Forwarding, at once
	CreateForward(ctx context.Context, f Forward, depositIDs []int64) (int64, error)
	SaveForward(ctx context.Context, f Forward) error
	// Marks a forward, and its deposits, as ended: confirmed (Forwarded)
	// or never relayed (back to Ready)
	EndForward(ctx context.Context, f Forward) error
	ForwardsIn(ctx context.Context, state string) ([]Forward, error)
	// The next lock address index
	NextLockIndex(ctx context.Context) (int64, error)
	GetLock(ctx context.Context, id int64) (Lock, error)
	// A lock of ours by txid (ErrNotFound: none)
	LockByTxid(ctx context.Context, txid string) (Lock, error)
	// A lock that was taken for failed went out after all: Sent again,
	// with these deposits Locking under it
	ReviveLock(ctx context.Context, lockID int64, depositIDs []int64) error
	SweepByTxid(ctx context.Context, txid string) (Sweep, error)
	// The same for a sweep and its locks
	ReviveSweep(ctx context.Context, sweepID int64, lockIDs []int64) error
	// Records a lock and marks its deposits Locking, at once
	CreateLock(ctx context.Context, l Lock, depositIDs []int64) (int64, error)
	SaveLock(ctx context.Context, l Lock) error
	// Marks a lock, and its deposits, as ended: confirmed (Locked) or never
	// relayed (back to Confirmed)
	EndLock(ctx context.Context, l Lock) error
	LocksIn(ctx context.Context, state string) ([]Lock, error)
	// Confirmed locks whose output no sweep spends
	Unswept(ctx context.Context) ([]Lock, error)
	Locks(ctx context.Context, requestID string) ([]Lock, error)
	// Records a sweep of locks' outputs and marks them, at once
	CreateSweep(ctx context.Context, s Sweep, lockIDs []int64) (int64, error)
	SaveSweep(ctx context.Context, s Sweep) error
	// Ends a sweep: confirmed, or never relayed (its locks unswept again)
	EndSweep(ctx context.Context, s Sweep) error
	SweepsIn(ctx context.Context, state string) ([]Sweep, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Settings
const (
	SettingLastPass  = "last_pass"
	SettingLastSweep = "last_sweep"
)

// Schema is created by the worker (the database's owner). The API's role
// may read everything and insert only a request's first columns and hints.
const Schema = `
CREATE TABLE IF NOT EXISTS sha256_requests (
    id text PRIMARY KEY,
    return_address text NOT NULL DEFAULT '',
    replay boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    key_index bigint UNIQUE,
    address text UNIQUE,
    last_polled timestamptz
);
CREATE TABLE IF NOT EXISTS sha256_forwards (
    id bigserial PRIMARY KEY,
    request_id text NOT NULL DEFAULT '',
    txid text NOT NULL UNIQUE,
    raw text NOT NULL,
    dest text NOT NULL,
    amount_sat bigint NOT NULL,
    fee_sat bigint NOT NULL,
    state text NOT NULL,
    confirmations bigint NOT NULL DEFAULT 0,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sha256_deposits (
    id bigserial PRIMARY KEY,
    request_id text NOT NULL REFERENCES sha256_requests (id),
    chain text NOT NULL,
    txid text NOT NULL,
    vout bigint NOT NULL,
    value_sat bigint NOT NULL,
    confirmations bigint NOT NULL DEFAULT 0,
    state text NOT NULL,
    forward_id bigint REFERENCES sha256_forwards (id),
    copy_state text NOT NULL DEFAULT '',
    copy_reason text NOT NULL DEFAULT '',
    missing int NOT NULL DEFAULT 0,
    first_seen timestamptz NOT NULL DEFAULT now(),
    UNIQUE (chain, txid, vout)
);
CREATE INDEX IF NOT EXISTS sha256_deposits_request ON sha256_deposits (request_id);
CREATE INDEX IF NOT EXISTS sha256_deposits_state ON sha256_deposits (chain, state);
CREATE TABLE IF NOT EXISTS sha256_hints (
    id bigserial PRIMARY KEY,
    request_id text NOT NULL REFERENCES sha256_requests (id),
    txid text NOT NULL,
    proofs jsonb NOT NULL DEFAULT '[]',
    at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sha256_hints_request ON sha256_hints (request_id, txid);
CREATE TABLE IF NOT EXISTS sha256_sweeps (
    id bigserial PRIMARY KEY,
    txid text NOT NULL UNIQUE,
    raw text NOT NULL,
    dest text NOT NULL,
    amount_sat bigint NOT NULL,
    fee_sat bigint NOT NULL,
    state text NOT NULL,
    confirmations bigint NOT NULL DEFAULT 0,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sha256_deposits ADD COLUMN IF NOT EXISTS sweep_id bigint REFERENCES sha256_sweeps (id);
CREATE TABLE IF NOT EXISTS sha256_locks (
    id bigserial PRIMARY KEY,
    txid text NOT NULL UNIQUE,
    raw text NOT NULL,
    key_index bigint NOT NULL UNIQUE,
    value_sat bigint NOT NULL,
    fee_sat bigint NOT NULL,
    state text NOT NULL,
    confirmations bigint NOT NULL DEFAULT 0,
    error text NOT NULL DEFAULT '',
    sweep_id bigint REFERENCES sha256_sweeps (id),
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sha256_deposits ADD COLUMN IF NOT EXISTS lock_id bigint REFERENCES sha256_locks (id);
ALTER TABLE sha256_deposits ADD COLUMN IF NOT EXISTS hold text NOT NULL DEFAULT '';
ALTER TABLE sha256_deposits ADD COLUMN IF NOT EXISTS hold_checked timestamptz;
CREATE TABLE IF NOT EXISTS sha256_settings (key text PRIMARY KEY, value text NOT NULL);
DO $$ BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_api') THEN
    GRANT INSERT (id, return_address, replay) ON sha256_requests TO donations_api;
    GRANT INSERT (request_id, txid, proofs) ON sha256_hints TO donations_api;
    GRANT USAGE ON SEQUENCE sha256_hints_id_seq TO donations_api;
  END IF;
END $$;
`

// Postgres is the production store.
type Postgres struct{ Pool *pgxpool.Pool }

func (p *Postgres) CreateRequest(ctx context.Context, r Request) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO sha256_requests (id, return_address, replay)
        VALUES ($1, $2, $3)`, r.ID, r.ReturnAddress, r.Replay)
	if err == nil {
		_, _ = p.Pool.Exec(ctx, `SELECT pg_notify('sha256_requests', $1)`, r.ID)
	}
	return err
}

func (p *Postgres) CreateRequestWithin(ctx context.Context, r Request, since time.Time, max int) (bool, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One creation at a time, so the count holds until the insert
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(52437)`); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sha256_requests WHERE created_at > $1`, since).Scan(&n); err != nil {
		return false, err
	}
	if n >= max {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sha256_requests (id, return_address, replay)
        VALUES ($1, $2, $3)`, r.ID, r.ReturnAddress, r.Replay); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	_, _ = p.Pool.Exec(ctx, `SELECT pg_notify('sha256_requests', $1)`, r.ID)
	return true, nil
}

func (p *Postgres) IsRequestAddress(ctx context.Context, address string) (bool, error) {
	var ok bool
	err := p.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sha256_requests WHERE address = $1)`,
		address).Scan(&ok)
	return ok, err
}

const requestColumns = `id, return_address, replay, created_at, coalesce(address, ''),
    coalesce(key_index, -1), last_polled`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.ReturnAddress, &r.Replay, &r.CreatedAt, &r.Address,
		&r.KeyIndex, &r.LastPolled)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (p *Postgres) requests(ctx context.Context, where string, args ...any) ([]Request, error) {
	rows, err := p.Pool.Query(ctx, `SELECT `+requestColumns+` FROM sha256_requests `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) GetRequest(ctx context.Context, id string) (Request, error) {
	return scanRequest(p.Pool.QueryRow(ctx, `SELECT `+requestColumns+
		` FROM sha256_requests WHERE id = $1`, id))
}

func (p *Postgres) Unassigned(ctx context.Context) ([]Request, error) {
	return p.requests(ctx, `WHERE address IS NULL ORDER BY created_at`)
}

func (p *Postgres) Assigned(ctx context.Context) ([]Request, error) {
	return p.requests(ctx, `WHERE address IS NOT NULL ORDER BY last_polled NULLS FIRST, created_at`)
}

func (p *Postgres) Assign(ctx context.Context, id string, address func(int64) (string, error)) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One worker; the lock keeps two passes from taking the same index
	if _, err := tx.Exec(ctx, `LOCK TABLE sha256_requests IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var next int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(key_index) + 1, 0) FROM sha256_requests`).Scan(&next); err != nil {
		return err
	}
	addr, err := address(next)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sha256_requests SET key_index = $2, address = $3
        WHERE id = $1 AND address IS NULL`, id, next, addr); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Due: PollEvery in SQL (every pass the first day, 10 minutes the first
// week, hourly the first month, daily the first year), or a newer hint
func (p *Postgres) Due(ctx context.Context, now time.Time, limit int) ([]Request, error) {
	return p.requests(ctx, fmt.Sprintf(`WHERE address IS NOT NULL AND (
        last_polled IS NULL
        OR created_at > $1::timestamptz - interval '1 day'
        OR (created_at > $1::timestamptz - interval '7 days' AND last_polled < $1::timestamptz - interval '10 minutes')
        OR (created_at > $1::timestamptz - interval '30 days' AND last_polled < $1::timestamptz - interval '1 hour')
        OR (created_at > $1::timestamptz - interval '365 days' AND last_polled < $1::timestamptz - interval '1 day')
        OR EXISTS (SELECT 1 FROM sha256_hints h WHERE h.request_id = sha256_requests.id AND h.at > last_polled))
        ORDER BY last_polled NULLS FIRST, created_at LIMIT %d`, limit), now)
}

func (p *Postgres) Polled(ctx context.Context, id string, at time.Time) error {
	_, err := p.Pool.Exec(ctx, `UPDATE sha256_requests SET last_polled = $2 WHERE id = $1`, id, at)
	return err
}

func (p *Postgres) CreatedSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM sha256_requests WHERE created_at > $1`,
		since).Scan(&n)
	return n, err
}

const depositColumns = `id, request_id, chain, txid, vout, value_sat, confirmations, state,
    coalesce(forward_id, 0), coalesce(lock_id, 0), copy_state, copy_reason, missing, first_seen,
    hold, hold_checked`

func (p *Postgres) deposits(ctx context.Context, where string, args ...any) ([]Deposit, error) {
	rows, err := p.Pool.Query(ctx, `SELECT `+depositColumns+` FROM sha256_deposits `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		var d Deposit
		var vout int64
		if err := rows.Scan(&d.ID, &d.RequestID, &d.Chain, &d.Txid, &vout, &d.ValueSat,
			&d.Confirmations, &d.State, &d.ForwardID, &d.LockID, &d.CopyState, &d.CopyReason,
			&d.Missing, &d.FirstSeen, &d.Hold, &d.HoldChecked); err != nil {
			return nil, err
		}
		d.Vout = uint32(vout)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) Deposits(ctx context.Context, requestID string) ([]Deposit, error) {
	return p.deposits(ctx, `WHERE request_id = $1 ORDER BY id`, requestID)
}

func (p *Postgres) DepositsIn(ctx context.Context, chain string, states ...string) ([]Deposit, error) {
	return p.deposits(ctx, `WHERE chain = $1 AND state = ANY($2) ORDER BY id`, chain, states)
}

func (p *Postgres) PutDeposit(ctx context.Context, d Deposit) (Deposit, error) {
	_, err := p.Pool.Exec(ctx, `INSERT INTO sha256_deposits (request_id, chain, txid, vout,
        value_sat, confirmations, state) VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (chain, txid, vout) DO UPDATE SET confirmations = excluded.confirmations,
        missing = 0, state = CASE WHEN sha256_deposits.state = 'gone' THEN 'seen'
        ELSE sha256_deposits.state END`, d.RequestID, d.Chain, d.Txid, int64(d.Vout), d.ValueSat, d.Confirmations, d.State)
	if err != nil {
		return d, err
	}
	got, err := p.deposits(ctx, `WHERE chain = $1 AND txid = $2 AND vout = $3`, d.Chain, d.Txid, int64(d.Vout))
	if err != nil || len(got) != 1 {
		return d, errors.Join(err, errors.New("deposit not stored"))
	}
	return got[0], nil
}

func (p *Postgres) SaveDeposit(ctx context.Context, d Deposit) error {
	_, err := p.Pool.Exec(ctx, `UPDATE sha256_deposits SET confirmations = $2, state = $3,
        copy_state = $4, copy_reason = $5, missing = $6, hold = $7, hold_checked = $8 WHERE id = $1`,
		d.ID, d.Confirmations, d.State, d.CopyState, d.CopyReason, d.Missing, d.Hold, d.HoldChecked)
	return err
}

func (p *Postgres) AddHint(ctx context.Context, h Hint) error {
	proofs, err := json.Marshal(h.Proofs)
	if err != nil {
		return err
	}
	_, err = p.Pool.Exec(ctx, `INSERT INTO sha256_hints (request_id, txid, proofs)
        VALUES ($1, $2, $3)`, h.RequestID, h.Txid, proofs)
	if err == nil {
		_, _ = p.Pool.Exec(ctx, `SELECT pg_notify('sha256_requests', $1)`, h.RequestID)
	}
	return err
}

func (p *Postgres) Hints(ctx context.Context, requestID, txid string) ([]Hint, error) {
	rows, err := p.Pool.Query(ctx, `SELECT id, request_id, txid, proofs, at FROM sha256_hints
        WHERE request_id = $1 AND ($2 = '' OR txid = $2) ORDER BY id`, requestID, txid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hint
	for rows.Next() {
		var h Hint
		var proofs []byte
		if err := rows.Scan(&h.ID, &h.RequestID, &h.Txid, &proofs, &h.At); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(proofs, &h.Proofs)
		out = append(out, h)
	}
	return out, rows.Err()
}

const forwardColumns = `id, request_id, txid, raw, dest, amount_sat, fee_sat, state,
    confirmations, error, created_at`

func (p *Postgres) forwards(ctx context.Context, where string, args ...any) ([]Forward, error) {
	rows, err := p.Pool.Query(ctx, `SELECT `+forwardColumns+` FROM sha256_forwards `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Forward
	for rows.Next() {
		var f Forward
		if err := rows.Scan(&f.ID, &f.RequestID, &f.Txid, &f.Raw, &f.Dest, &f.AmountSat,
			&f.FeeSat, &f.State, &f.Confirmations, &f.Error, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (p *Postgres) Forwards(ctx context.Context, requestID string) ([]Forward, error) {
	return p.forwards(ctx, `WHERE id IN (SELECT forward_id FROM sha256_deposits
        WHERE request_id = $1) ORDER BY id`, requestID)
}

func (p *Postgres) ForwardsIn(ctx context.Context, state string) ([]Forward, error) {
	return p.forwards(ctx, `WHERE state = $1 ORDER BY id`, state)
}

func (p *Postgres) CreateForward(ctx context.Context, f Forward, depositIDs []int64) (int64, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO sha256_forwards (request_id, txid, raw, dest,
        amount_sat, fee_sat, state) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		f.RequestID, f.Txid, f.Raw, f.Dest, f.AmountSat, f.FeeSat, f.State).Scan(&id); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2, forward_id = $3
        WHERE id = ANY($1) AND state = $4`, depositIDs, Forwarding, id, Ready)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != int64(len(depositIDs)) {
		return 0, errors.New("a deposit is no longer ready")
	}
	return id, tx.Commit(ctx)
}

func (p *Postgres) SaveForward(ctx context.Context, f Forward) error {
	_, err := p.Pool.Exec(ctx, `UPDATE sha256_forwards SET state = $2, confirmations = $3,
        error = $4 WHERE id = $1`, f.ID, f.State, f.Confirmations, f.Error)
	return err
}

func (p *Postgres) EndForward(ctx context.Context, f Forward) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sha256_forwards SET state = $2, confirmations = $3,
        error = $4 WHERE id = $1`, f.ID, f.State, f.Confirmations, f.Error); err != nil {
		return err
	}
	switch f.State {
	case Done:
		_, err = tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2 WHERE forward_id = $1`, f.ID, Forwarded)
	case FailedSend:
		_, err = tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2, forward_id = NULL
            WHERE forward_id = $1`, f.ID, Ready)
	default:
		err = errors.New("a forward ends confirmed or failed")
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const lockColumns = `id, txid, raw, key_index, value_sat, fee_sat, state, confirmations,
    error, coalesce(sweep_id, 0), created_at`

func (p *Postgres) locks(ctx context.Context, where string, args ...any) ([]Lock, error) {
	rows, err := p.Pool.Query(ctx, `SELECT `+lockColumns+` FROM sha256_locks `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lock
	for rows.Next() {
		var l Lock
		if err := rows.Scan(&l.ID, &l.Txid, &l.Raw, &l.KeyIndex, &l.ValueSat, &l.FeeSat,
			&l.State, &l.Confirmations, &l.Error, &l.SweepID, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (p *Postgres) GetLock(ctx context.Context, id int64) (Lock, error) {
	ls, err := p.locks(ctx, `WHERE id = $1`, id)
	if err != nil {
		return Lock{}, err
	}
	if len(ls) == 0 {
		return Lock{}, ErrNotFound
	}
	return ls[0], nil
}

func (p *Postgres) LockByTxid(ctx context.Context, txid string) (Lock, error) {
	ls, err := p.locks(ctx, `WHERE txid = $1`, txid)
	if err != nil {
		return Lock{}, err
	}
	if len(ls) == 0 {
		return Lock{}, ErrNotFound
	}
	return ls[0], nil
}

func (p *Postgres) ReviveLock(ctx context.Context, lockID int64, depositIDs []int64) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sha256_locks SET state = $2, error = '' WHERE id = $1`, lockID, Sent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2, lock_id = $3
        WHERE id = ANY($1) AND chain = 'sha256'`, depositIDs, Locking, lockID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) SweepByTxid(ctx context.Context, txid string) (Sweep, error) {
	var sw Sweep
	err := p.Pool.QueryRow(ctx, `SELECT id, txid, raw, dest, amount_sat, fee_sat, state,
        confirmations, error, created_at FROM sha256_sweeps WHERE txid = $1`, txid).Scan(
		&sw.ID, &sw.Txid, &sw.Raw, &sw.Dest, &sw.AmountSat, &sw.FeeSat, &sw.State,
		&sw.Confirmations, &sw.Error, &sw.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sw, ErrNotFound
	}
	return sw, err
}

func (p *Postgres) ReviveSweep(ctx context.Context, sweepID int64, lockIDs []int64) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sha256_sweeps SET state = $2, error = '' WHERE id = $1`, sweepID, Sent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sha256_locks SET sweep_id = $2 WHERE id = ANY($1)`, lockIDs, sweepID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) NextLockIndex(ctx context.Context) (int64, error) {
	var n int64
	err := p.Pool.QueryRow(ctx, `SELECT coalesce(max(key_index) + 1, 0) FROM sha256_locks`).Scan(&n)
	return n, err
}

func (p *Postgres) CreateLock(ctx context.Context, l Lock, depositIDs []int64) (int64, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO sha256_locks (txid, raw, key_index, value_sat,
        fee_sat, state) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		l.Txid, l.Raw, l.KeyIndex, l.ValueSat, l.FeeSat, l.State).Scan(&id); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2, lock_id = $3
        WHERE id = ANY($1) AND chain = 'sha256' AND state = $4`, depositIDs, Locking, id, Confirmed)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != int64(len(depositIDs)) {
		return 0, errors.New("a deposit is no longer confirmed and unlocked")
	}
	return id, tx.Commit(ctx)
}

func (p *Postgres) SaveLock(ctx context.Context, l Lock) error {
	_, err := p.Pool.Exec(ctx, `UPDATE sha256_locks SET state = $2, confirmations = $3,
        error = $4 WHERE id = $1`, l.ID, l.State, l.Confirmations, l.Error)
	return err
}

func (p *Postgres) EndLock(ctx context.Context, l Lock) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sha256_locks SET state = $2, confirmations = $3,
        error = $4 WHERE id = $1`, l.ID, l.State, l.Confirmations, l.Error); err != nil {
		return err
	}
	switch l.State {
	case Done:
		_, err = tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2 WHERE lock_id = $1`, l.ID, Locked)
	case FailedSend:
		_, err = tx.Exec(ctx, `UPDATE sha256_deposits SET state = $2, lock_id = NULL
            WHERE lock_id = $1`, l.ID, Confirmed)
	default:
		err = errors.New("a lock ends confirmed or failed")
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) LocksIn(ctx context.Context, state string) ([]Lock, error) {
	return p.locks(ctx, `WHERE state = $1 ORDER BY id`, state)
}

func (p *Postgres) Unswept(ctx context.Context) ([]Lock, error) {
	return p.locks(ctx, `WHERE state = $1 AND sweep_id IS NULL ORDER BY id`, Done)
}

func (p *Postgres) Locks(ctx context.Context, requestID string) ([]Lock, error) {
	return p.locks(ctx, `WHERE id IN (SELECT lock_id FROM sha256_deposits
        WHERE request_id = $1) ORDER BY id`, requestID)
}

func (p *Postgres) CreateSweep(ctx context.Context, sw Sweep, lockIDs []int64) (int64, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO sha256_sweeps (txid, raw, dest, amount_sat,
        fee_sat, state) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		sw.Txid, sw.Raw, sw.Dest, sw.AmountSat, sw.FeeSat, sw.State).Scan(&id); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE sha256_locks SET sweep_id = $2
        WHERE id = ANY($1) AND state = $3 AND sweep_id IS NULL`, lockIDs, id, Done)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != int64(len(lockIDs)) {
		return 0, errors.New("a lock is no longer confirmed and unswept")
	}
	return id, tx.Commit(ctx)
}

func (p *Postgres) SaveSweep(ctx context.Context, sw Sweep) error {
	_, err := p.Pool.Exec(ctx, `UPDATE sha256_sweeps SET state = $2, confirmations = $3,
        error = $4 WHERE id = $1`, sw.ID, sw.State, sw.Confirmations, sw.Error)
	return err
}

func (p *Postgres) EndSweep(ctx context.Context, sw Sweep) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sha256_sweeps SET state = $2, confirmations = $3,
        error = $4 WHERE id = $1`, sw.ID, sw.State, sw.Confirmations, sw.Error); err != nil {
		return err
	}
	switch sw.State {
	case Done:
	case FailedSend:
		if _, err := tx.Exec(ctx, `UPDATE sha256_locks SET sweep_id = NULL WHERE sweep_id = $1`, sw.ID); err != nil {
			return err
		}
	default:
		return errors.New("a sweep ends confirmed or failed")
	}
	return tx.Commit(ctx)
}

func (p *Postgres) SweepsIn(ctx context.Context, state string) ([]Sweep, error) {
	rows, err := p.Pool.Query(ctx, `SELECT id, txid, raw, dest, amount_sat, fee_sat, state,
        confirmations, error, created_at FROM sha256_sweeps WHERE state = $1 ORDER BY id`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sweep
	for rows.Next() {
		var sw Sweep
		if err := rows.Scan(&sw.ID, &sw.Txid, &sw.Raw, &sw.Dest, &sw.AmountSat, &sw.FeeSat,
			&sw.State, &sw.Confirmations, &sw.Error, &sw.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}

func (p *Postgres) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := p.Pool.QueryRow(ctx, `SELECT value FROM sha256_settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (p *Postgres) SetSetting(ctx context.Context, key, value string) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO sha256_settings (key, value) VALUES ($1, $2)
        ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Memory is a store for tests.
type Memory struct {
	mu       sync.Mutex
	requests map[string]*Request
	deposits []*Deposit
	forwards []*Forward
	sweeps   []*Sweep
	locks    []*Lock
	hints    []Hint
	settings map[string]string
	Now      func() time.Time
}

// NewMemory is an empty store.
func NewMemory() *Memory {
	return &Memory{requests: map[string]*Request{}, settings: map[string]string{},
		Now: time.Now}
}

func (m *Memory) CreateRequest(_ context.Context, r Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.requests[r.ID]; ok {
		return errors.New("duplicate")
	}
	r.CreatedAt, r.Address, r.KeyIndex = m.Now(), "", -1
	m.requests[r.ID] = &r
	return nil
}

func (m *Memory) CreateRequestWithin(ctx context.Context, r Request, since time.Time, max int) (bool, error) {
	m.mu.Lock()
	n := 0
	for _, e := range m.requests {
		if e.CreatedAt.After(since) {
			n++
		}
	}
	m.mu.Unlock()
	if n >= max {
		return false, nil
	}
	return true, m.CreateRequest(ctx, r)
}

func (m *Memory) IsRequestAddress(_ context.Context, address string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.Address == address {
			return true, nil
		}
	}
	return false, nil
}

func (m *Memory) GetRequest(_ context.Context, id string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.requests[id]; ok {
		return *r, nil
	}
	return Request{}, ErrNotFound
}

func (m *Memory) sorted(keep func(*Request) bool) []Request {
	var out []Request
	for _, r := range m.requests {
		if keep(r) {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].LastPolled, out[j].LastPolled
		if (a == nil) != (b == nil) {
			return a == nil
		}
		if a != nil && !a.Equal(*b) {
			return a.Before(*b)
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt) ||
			(out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID)
	})
	return out
}

func (m *Memory) Unassigned(context.Context) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(r *Request) bool { return r.Address == "" }), nil
}

func (m *Memory) Assigned(context.Context) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(r *Request) bool { return r.Address != "" }), nil
}

func (m *Memory) Assign(_ context.Context, id string, address func(int64) (string, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := int64(0)
	for _, r := range m.requests {
		if r.KeyIndex >= next {
			next = r.KeyIndex + 1
		}
	}
	addr, err := address(next)
	if err != nil {
		return err
	}
	if r, ok := m.requests[id]; ok && r.Address == "" {
		r.KeyIndex, r.Address = next, addr
	}
	return nil
}

func (m *Memory) Due(_ context.Context, now time.Time, limit int) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hinted := func(r *Request) bool {
		for _, h := range m.hints {
			if h.RequestID == r.ID && h.At.After(*r.LastPolled) {
				return true
			}
		}
		return false
	}
	out := m.sorted(func(r *Request) bool {
		if r.Address == "" {
			return false
		}
		if r.LastPolled == nil || hinted(r) {
			return true
		}
		every := PollEvery(now.Sub(r.CreatedAt))
		return every >= 0 && now.Sub(*r.LastPolled) >= every
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Polled(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.requests[id]; ok {
		r.LastPolled = &at
	}
	return nil
}

func (m *Memory) CreatedSince(_ context.Context, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.requests {
		if r.CreatedAt.After(since) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) Deposits(_ context.Context, requestID string) ([]Deposit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Deposit
	for _, d := range m.deposits {
		if d.RequestID == requestID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (m *Memory) DepositsIn(_ context.Context, chain string, states ...string) ([]Deposit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Deposit
	for _, d := range m.deposits {
		for _, s := range states {
			if d.Chain == chain && d.State == s {
				out = append(out, *d)
			}
		}
	}
	return out, nil
}

func (m *Memory) PutDeposit(_ context.Context, d Deposit) (Deposit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.deposits {
		if e.Chain == d.Chain && e.Txid == d.Txid && e.Vout == d.Vout {
			e.Confirmations, e.Missing = d.Confirmations, 0
			if e.State == Gone {
				e.State = Seen
			}
			return *e, nil
		}
	}
	d.ID = int64(len(m.deposits) + 1)
	d.FirstSeen = m.Now()
	m.deposits = append(m.deposits, &d)
	return d, nil
}

func (m *Memory) SaveDeposit(_ context.Context, d Deposit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.deposits {
		if e.ID == d.ID {
			e.Confirmations, e.State, e.CopyState, e.CopyReason, e.Missing, e.Hold, e.HoldChecked =
				d.Confirmations, d.State, d.CopyState, d.CopyReason, d.Missing, d.Hold, d.HoldChecked
		}
	}
	return nil
}

func (m *Memory) AddHint(_ context.Context, h Hint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h.ID, h.At = int64(len(m.hints)+1), m.Now()
	m.hints = append(m.hints, h)
	return nil
}

func (m *Memory) Hints(_ context.Context, requestID, txid string) ([]Hint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Hint
	for _, h := range m.hints {
		if h.RequestID == requestID && (txid == "" || h.Txid == txid) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (m *Memory) Forwards(_ context.Context, requestID string) ([]Forward, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := map[int64]bool{}
	for _, d := range m.deposits {
		if d.RequestID == requestID && d.ForwardID != 0 {
			ids[d.ForwardID] = true
		}
	}
	var out []Forward
	for _, f := range m.forwards {
		if ids[f.ID] {
			out = append(out, *f)
		}
	}
	return out, nil
}

func (m *Memory) ForwardsIn(_ context.Context, state string) ([]Forward, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Forward
	for _, f := range m.forwards {
		if f.State == state {
			out = append(out, *f)
		}
	}
	return out, nil
}

func (m *Memory) CreateForward(_ context.Context, f Forward, depositIDs []int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var picked []*Deposit
	for _, id := range depositIDs {
		for _, d := range m.deposits {
			if d.ID == id && d.State == Ready {
				picked = append(picked, d)
			}
		}
	}
	if len(picked) != len(depositIDs) {
		return 0, errors.New("a deposit is no longer ready")
	}
	f.ID, f.CreatedAt = int64(len(m.forwards)+1), m.Now()
	m.forwards = append(m.forwards, &f)
	for _, d := range picked {
		d.State, d.ForwardID = Forwarding, f.ID
	}
	return f.ID, nil
}

func (m *Memory) SaveForward(_ context.Context, f Forward) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.forwards {
		if e.ID == f.ID {
			e.State, e.Confirmations, e.Error = f.State, f.Confirmations, f.Error
		}
	}
	return nil
}

func (m *Memory) EndForward(ctx context.Context, f Forward) error {
	if f.State != Done && f.State != FailedSend {
		return errors.New("a forward ends confirmed or failed")
	}
	if err := m.SaveForward(ctx, f); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deposits {
		if d.ForwardID != f.ID {
			continue
		}
		if f.State == Done {
			d.State = Forwarded
		} else {
			d.State, d.ForwardID = Ready, 0
		}
	}
	return nil
}

func (m *Memory) GetLock(_ context.Context, id int64) (Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.locks {
		if l.ID == id {
			return *l, nil
		}
	}
	return Lock{}, ErrNotFound
}

func (m *Memory) LockByTxid(_ context.Context, txid string) (Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.locks {
		if l.Txid == txid {
			return *l, nil
		}
	}
	return Lock{}, ErrNotFound
}

func (m *Memory) ReviveLock(_ context.Context, lockID int64, depositIDs []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.locks {
		if l.ID == lockID {
			l.State, l.Error = Sent, ""
		}
	}
	for _, id := range depositIDs {
		for _, d := range m.deposits {
			if d.ID == id && d.Chain == SHA256 {
				d.State, d.LockID = Locking, lockID
			}
		}
	}
	return nil
}

func (m *Memory) SweepByTxid(_ context.Context, txid string) (Sweep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sw := range m.sweeps {
		if sw.Txid == txid {
			return *sw, nil
		}
	}
	return Sweep{}, ErrNotFound
}

func (m *Memory) ReviveSweep(_ context.Context, sweepID int64, lockIDs []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sw := range m.sweeps {
		if sw.ID == sweepID {
			sw.State, sw.Error = Sent, ""
		}
	}
	for _, id := range lockIDs {
		for _, l := range m.locks {
			if l.ID == id {
				l.SweepID = sweepID
			}
		}
	}
	return nil
}

func (m *Memory) NextLockIndex(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := int64(0)
	for _, l := range m.locks {
		if l.KeyIndex >= n {
			n = l.KeyIndex + 1
		}
	}
	return n, nil
}

func (m *Memory) CreateLock(_ context.Context, l Lock, depositIDs []int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var picked []*Deposit
	for _, id := range depositIDs {
		for _, d := range m.deposits {
			if d.ID == id && d.Chain == SHA256 && d.State == Confirmed {
				picked = append(picked, d)
			}
		}
	}
	if len(picked) != len(depositIDs) {
		return 0, errors.New("a deposit is no longer confirmed and unlocked")
	}
	l.ID, l.CreatedAt = int64(len(m.locks)+1), m.Now()
	m.locks = append(m.locks, &l)
	for _, d := range picked {
		d.State, d.LockID = Locking, l.ID
	}
	return l.ID, nil
}

func (m *Memory) SaveLock(_ context.Context, l Lock) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.locks {
		if e.ID == l.ID {
			e.State, e.Confirmations, e.Error = l.State, l.Confirmations, l.Error
		}
	}
	return nil
}

func (m *Memory) EndLock(ctx context.Context, l Lock) error {
	if l.State != Done && l.State != FailedSend {
		return errors.New("a lock ends confirmed or failed")
	}
	if err := m.SaveLock(ctx, l); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deposits {
		if d.LockID != l.ID {
			continue
		}
		if l.State == Done {
			d.State = Locked
		} else {
			d.State, d.LockID = Confirmed, 0
		}
	}
	return nil
}

func (m *Memory) LocksIn(_ context.Context, state string) ([]Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Lock
	for _, l := range m.locks {
		if l.State == state {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (m *Memory) Unswept(_ context.Context) ([]Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Lock
	for _, l := range m.locks {
		if l.State == Done && l.SweepID == 0 {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (m *Memory) Locks(_ context.Context, requestID string) ([]Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := map[int64]bool{}
	for _, d := range m.deposits {
		if d.RequestID == requestID && d.LockID != 0 {
			ids[d.LockID] = true
		}
	}
	var out []Lock
	for _, l := range m.locks {
		if ids[l.ID] {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (m *Memory) CreateSweep(_ context.Context, sw Sweep, lockIDs []int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var picked []*Lock
	for _, id := range lockIDs {
		for _, l := range m.locks {
			if l.ID == id && l.State == Done && l.SweepID == 0 {
				picked = append(picked, l)
			}
		}
	}
	if len(picked) != len(lockIDs) {
		return 0, errors.New("a lock is no longer confirmed and unswept")
	}
	sw.ID, sw.CreatedAt = int64(len(m.sweeps)+1), m.Now()
	m.sweeps = append(m.sweeps, &sw)
	for _, l := range picked {
		l.SweepID = sw.ID
	}
	return sw.ID, nil
}

func (m *Memory) SaveSweep(_ context.Context, sw Sweep) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.sweeps {
		if e.ID == sw.ID {
			e.State, e.Confirmations, e.Error = sw.State, sw.Confirmations, sw.Error
		}
	}
	return nil
}

func (m *Memory) EndSweep(ctx context.Context, sw Sweep) error {
	if sw.State != Done && sw.State != FailedSend {
		return errors.New("a sweep ends confirmed or failed")
	}
	if err := m.SaveSweep(ctx, sw); err != nil {
		return err
	}
	if sw.State == FailedSend {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, l := range m.locks {
			if l.SweepID == sw.ID {
				l.SweepID = 0
			}
		}
	}
	return nil
}

func (m *Memory) SweepsIn(_ context.Context, state string) ([]Sweep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Sweep
	for _, sw := range m.sweeps {
		if sw.State == state {
			out = append(out, *sw)
		}
	}
	return out, nil
}

func (m *Memory) Setting(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings[key], nil
}

func (m *Memory) SetSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[key] = value
	return nil
}
