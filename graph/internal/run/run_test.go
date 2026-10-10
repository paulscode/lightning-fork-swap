package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/graph/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/graph/internal/tiles"
)

type source struct {
	info  lnd.Info
	graph map[string]any
	fail  error
}

func (s *source) GetInfo(context.Context) (lnd.Info, error) { return s.info, s.fail }
func (s *source) DescribeGraph(context.Context) ([]byte, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	return json.Marshal(s.graph)
}

func key(i int) string { return fmt.Sprintf("02%064x", i) }

func fake() *source {
	var nodes, edges []map[string]any
	for i := 0; i < 6; i++ {
		nodes = append(nodes, map[string]any{"pub_key": key(i), "alias": fmt.Sprintf("n%d", i),
			"color": "#3399ff", "addresses": []map[string]string{{"addr": "1.2.3.4:9735"}}})
	}
	for i := 1; i < 6; i++ {
		edges = append(edges, map[string]any{"channel_id": fmt.Sprint(1000 + i),
			"node1_pub": key(0), "node2_pub": key(i), "capacity": "2000000"})
	}
	// Junk lnd could send: a bad key, a channel to nobody, no capacity
	nodes = append(nodes, map[string]any{"pub_key": "zz"})
	edges = append(edges, map[string]any{"channel_id": "1", "node1_pub": key(0), "node2_pub": key(77), "capacity": "5"},
		map[string]any{"channel_id": "2", "node1_pub": key(0), "node2_pub": key(1), "capacity": "0"})
	return &source{info: lnd.Info{IdentityPubkey: key(0), URIs: []string{key(0) + "@1.2.3.4:9735"}},
		graph: map[string]any{"nodes": nodes, "edges": edges}}
}

func TestRounds(t *testing.T) {
	dir := t.TempDir()
	out, state := filepath.Join(dir, "out"), filepath.Join(dir, "state", "state.json")
	src := fake()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	v, fresh, err := Round(context.Background(), src, out, state, now)
	if err != nil || v != 1 || !fresh {
		t.Fatalf("%d %v %v", v, fresh, err)
	}
	meta, _ := tiles.ReadMeta(out)
	if meta.Nodes != 6 || meta.Channels != 5 || meta.Capacity != 10_000_000 ||
		meta.Ours.Pubkey != key(0) || len(meta.Ours.URIs) != 1 {
		t.Fatalf("%+v", meta)
	}
	first := LoadState(state).Positions

	// Nothing changed: no new version, but meta says it was checked
	later := now.Add(10 * time.Minute)
	v, fresh, err = Round(context.Background(), src, out, state, later)
	if err != nil || v != 1 || fresh {
		t.Fatalf("%d %v %v", v, fresh, err)
	}
	if meta, _ = tiles.ReadMeta(out); !meta.CheckedAt.Equal(later) || !meta.GeneratedAt.Equal(now) {
		t.Fatalf("%+v", meta)
	}

	// An alias changes: a new version, the layout kept
	src.graph["nodes"].([]map[string]any)[3]["alias"] = "renamed"
	v, fresh, err = Round(context.Background(), src, out, state, later.Add(time.Minute))
	if err != nil || v != 2 || !fresh {
		t.Fatalf("%d %v %v", v, fresh, err)
	}
	second := LoadState(state).Positions
	for k, p := range first {
		q := second[k]
		d := 0.0
		for i := 0; i < 3; i++ {
			d += (p[i] - q[i]) * (p[i] - q[i])
		}
		if d > 1 {
			t.Fatalf("%s moved %.2f", k[:8], d)
		}
	}

	src.fail = errors.New("lnd down")
	if _, _, err := Round(context.Background(), src, out, state, later); err == nil {
		t.Fatal("no error")
	}
	if meta, _ := tiles.ReadMeta(out); meta.Version != 2 {
		t.Fatal("meta changed on an error")
	}
}
