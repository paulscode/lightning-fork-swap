package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
)

// Memory is a Store in memory, for tests.
type Memory struct {
	mu      sync.Mutex
	Records map[string]*Record
	state   map[string]string
	Fail    error
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{Records: map[string]*Record{}, state: map[string]string{}}
}

func (m *Memory) AddDonation(_ context.Context, txid, address string, amount int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.Records[txid]; !ok {
		now := time.Now()
		m.Records[txid] = &Record{Txid: txid, Address: address,
			AmountSat: amount, FirstSeen: now, NextCheck: &now,
			Verdict: replay.Pending}
	}
	return nil
}

func (m *Memory) Due(_ context.Context, now time.Time, limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []Record
	for _, r := range m.Records {
		if r.NextCheck != nil && !r.NextCheck.After(now) {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextCheck.Before(*out[j].NextCheck) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Save(_ context.Context, txid string, r replay.Result, checked, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.Records[txid]
	rec.LastChecked = &checked
	if next.IsZero() {
		rec.NextCheck = nil
	} else {
		rec.NextCheck = &next
	}
	rec.Verdict, rec.Reason, rec.Inputs = r.Verdict, r.Reason, r.Inputs
	rec.AtRiskAddresses = r.AtRiskAddresses
	if r.ReplayedHeight != 0 {
		h := r.ReplayedHeight
		rec.ReplayedHeight = &h
	}
	return nil
}

func (m *Memory) Get(_ context.Context, txid string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return Record{}, m.Fail
	}
	if r, ok := m.Records[txid]; ok {
		return *r, nil
	}
	return Record{}, ErrNotFound
}

func (m *Memory) List(_ context.Context, verdicts []replay.Verdict) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.Records {
		for _, v := range verdicts {
			if r.Verdict == v {
				out = append(out, *r)
			}
		}
		if len(verdicts) == 0 {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (m *Memory) SetNote(_ context.Context, txid, note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.Records[txid]
	if !ok {
		return ErrNotFound
	}
	r.Notes = note
	return nil
}

func (m *Memory) State(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state[key], nil
}

func (m *Memory) SetState(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[key] = value
	return nil
}
