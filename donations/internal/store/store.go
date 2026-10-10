// Package store keeps the donation watcher's records in Postgres: one row
// per donation transaction with its replay check.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
)

// Schema is created by the worker (the owner); the API's role may read.
const Schema = `
CREATE TABLE IF NOT EXISTS replay_checks (
    txid text PRIMARY KEY,
    address text NOT NULL,
    amount_sat bigint NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now(),
    last_checked timestamptz,
    next_check timestamptz DEFAULT now(),
    verdict text NOT NULL DEFAULT 'pending',
    reason text NOT NULL DEFAULT '',
    inputs jsonb NOT NULL DEFAULT '[]',
    at_risk_addresses text[] NOT NULL DEFAULT '{}',
    replayed_height bigint,
    notes text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS replay_checks_next ON replay_checks (next_check);
CREATE INDEX IF NOT EXISTS replay_checks_verdict ON replay_checks (verdict);
CREATE TABLE IF NOT EXISTS worker_state (key text PRIMARY KEY, value text NOT NULL);
`

// Record is one donation transaction and what its last check found.
type Record struct {
	Txid            string
	Address         string
	AmountSat       int64
	FirstSeen       time.Time
	LastChecked     *time.Time
	NextCheck       *time.Time
	Verdict         replay.Verdict
	Reason          string
	Inputs          []replay.Input
	AtRiskAddresses []string
	ReplayedHeight  *int64
	Notes           string
}

// ErrNotFound: no such donation transaction.
var ErrNotFound = errors.New("not found")

// Store is what the worker, the API and the admin commands use.
type Store interface {
	AddDonation(ctx context.Context, txid, address string, amountSat int64) error
	Due(ctx context.Context, now time.Time, limit int) ([]Record, error)
	Save(ctx context.Context, txid string, r replay.Result, checked time.Time, next time.Time) error
	Get(ctx context.Context, txid string) (Record, error)
	List(ctx context.Context, verdicts []replay.Verdict) ([]Record, error)
	SetNote(ctx context.Context, txid, note string) error
	State(ctx context.Context, key string) (string, error)
	SetState(ctx context.Context, key, value string) error
}

// Postgres is the store.
type Postgres struct{ Pool *pgxpool.Pool }

// Open connects; the worker also creates the schema.
func Open(ctx context.Context, url string, createSchema bool) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if createSchema {
		if _, err := pool.Exec(ctx, Schema); err != nil {
			pool.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return &Postgres{Pool: pool}, nil
}

func (p *Postgres) AddDonation(ctx context.Context, txid, address string, amountSat int64) error {
	_, err := p.Pool.Exec(ctx,
		`INSERT INTO replay_checks (txid, address, amount_sat) VALUES ($1, $2, $3)
		 ON CONFLICT (txid) DO NOTHING`, txid, address, amountSat)
	return err
}

const columns = `txid, address, amount_sat, first_seen, last_checked, next_check,
	verdict, reason, inputs, at_risk_addresses, replayed_height, notes`

func scan(row pgx.Row) (Record, error) {
	var r Record
	var verdict string
	var inputs []byte
	err := row.Scan(&r.Txid, &r.Address, &r.AmountSat, &r.FirstSeen,
		&r.LastChecked, &r.NextCheck, &verdict, &r.Reason, &inputs,
		&r.AtRiskAddresses, &r.ReplayedHeight, &r.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.Verdict = replay.Verdict(verdict)
	if err := json.Unmarshal(inputs, &r.Inputs); err != nil {
		return r, err
	}
	return r, nil
}

func (p *Postgres) query(ctx context.Context, sql string, args ...any) ([]Record, error) {
	rows, err := p.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) Due(ctx context.Context, now time.Time, limit int) ([]Record, error) {
	return p.query(ctx, `SELECT `+columns+` FROM replay_checks
		WHERE next_check IS NOT NULL AND next_check <= $1
		ORDER BY next_check LIMIT $2`, now, limit)
}

func (p *Postgres) Save(ctx context.Context, txid string, r replay.Result, checked, next time.Time) error {
	inputs, err := json.Marshal(r.Inputs)
	if err != nil {
		return err
	}
	var nextCheck *time.Time
	if !next.IsZero() {
		nextCheck = &next
	}
	var height *int64
	if r.ReplayedHeight != 0 {
		height = &r.ReplayedHeight
	}
	addresses := r.AtRiskAddresses
	if addresses == nil {
		addresses = []string{}
	}
	_, err = p.Pool.Exec(ctx, `UPDATE replay_checks SET last_checked = $2,
		next_check = $3, verdict = $4, reason = $5, inputs = $6,
		at_risk_addresses = $7, replayed_height = $8 WHERE txid = $1`,
		txid, checked, nextCheck, string(r.Verdict), r.Reason, inputs,
		addresses, height)
	return err
}

func (p *Postgres) Get(ctx context.Context, txid string) (Record, error) {
	return scan(p.Pool.QueryRow(ctx, `SELECT `+columns+
		` FROM replay_checks WHERE txid = $1`, txid))
}

func (p *Postgres) List(ctx context.Context, verdicts []replay.Verdict) ([]Record, error) {
	if len(verdicts) == 0 {
		return p.query(ctx, `SELECT `+columns+` FROM replay_checks ORDER BY first_seen`)
	}
	names := make([]string, len(verdicts))
	for i, v := range verdicts {
		names[i] = string(v)
	}
	return p.query(ctx, `SELECT `+columns+` FROM replay_checks
		WHERE verdict = ANY($1) ORDER BY first_seen`, names)
}

func (p *Postgres) SetNote(ctx context.Context, txid, note string) error {
	tag, err := p.Pool.Exec(ctx, `UPDATE replay_checks SET notes = $2 WHERE txid = $1`, txid, note)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) State(ctx context.Context, key string) (string, error) {
	var value string
	err := p.Pool.QueryRow(ctx, `SELECT value FROM worker_state WHERE key = $1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (p *Postgres) SetState(ctx context.Context, key, value string) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO worker_state VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	return err
}
