// Package run is one round of the generator: read the graph, and when it
// changed, lay it out (from the last positions), find its communities and
// write the next version of the files.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/paulscode/lightning-fork-swap/graph/internal/cluster"
	"github.com/paulscode/lightning-fork-swap/graph/internal/layout"
	"github.com/paulscode/lightning-fork-swap/graph/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
	"github.com/paulscode/lightning-fork-swap/graph/internal/tiles"
)

// Source is lnd, or a fake.
type Source interface {
	GetInfo(ctx context.Context) (lnd.Info, error)
	DescribeGraph(ctx context.Context) ([]byte, error)
}

// State is what the generator keeps between rounds: the layout, so the sky
// keeps its shape, and what the last version was made of.
type State struct {
	Version   int                   `json:"version"`
	Hash      string                `json:"hash"`
	Positions map[string]layout.Vec `json:"positions"`
}

// LoadState reads the state, or an empty one.
func LoadState(path string) State {
	var s State
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Positions == nil {
		s.Positions = map[string]layout.Vec{}
	}
	return s
}

// Save writes the state atomically.
func (s State) Save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Hash is what a version depends on: the nodes as shown and the channels.
func Hash(g *model.Graph, ours lnd.Info) string {
	h := sha256.New()
	fmt.Fprintln(h, ours.IdentityPubkey, ours.URIs)
	for _, n := range g.Nodes {
		fmt.Fprintln(h, n.Pubkey, n.Alias, n.Color, n.Addresses)
	}
	for _, e := range g.Edges {
		fmt.Fprintln(h, e.ID, e.A, e.B, e.Capacity)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Round reads the graph and writes a new version when it changed. It
// returns the version now current and whether it is new.
func Round(ctx context.Context, src Source, out, statePath string, now time.Time) (int, bool, error) {
	info, err := src.GetInfo(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("getinfo: %w", err)
	}
	raw, err := src.DescribeGraph(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("describegraph: %w", err)
	}
	g, err := model.Parse(raw)
	if err != nil {
		return 0, false, fmt.Errorf("graph: %w", err)
	}
	state := LoadState(statePath)
	hash := Hash(g, info)
	if meta, ok := tiles.ReadMeta(out); ok && meta.Version == state.Version && hash == state.Hash {
		return meta.Version, false, tiles.Touch(out, meta, now)
	}
	pos := layout.Run(g, state.Positions, info.IdentityPubkey, layout.DefaultOptions())
	comm, k := cluster.Communities(g)
	version := state.Version + 1
	if meta, ok := tiles.ReadMeta(out); ok && meta.Version >= version {
		version = meta.Version + 1
	}
	if _, err := tiles.Write(out, tiles.Input{Graph: g, Pos: pos, Communities: comm, K: k,
		Ours: tiles.Ours{Pubkey: info.IdentityPubkey, URIs: info.URIs}, Version: version,
		Now: now}); err != nil {
		return 0, false, err
	}
	positions := make(map[string]layout.Vec, len(g.Nodes))
	for i, n := range g.Nodes {
		positions[n.Pubkey] = pos[i]
	}
	if err := (State{Version: version, Hash: hash, Positions: positions}).Save(statePath); err != nil {
		return 0, false, err
	}
	return version, true, nil
}
