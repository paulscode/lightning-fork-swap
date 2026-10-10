package tiles

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/graph/internal/cluster"
	"github.com/paulscode/lightning-fork-swap/graph/internal/layout"
	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
	"github.com/paulscode/lightning-fork-swap/graph/internal/synthetic"
)

func input(g *model.Graph, version int) Input {
	pos := layout.Run(g, nil, g.Nodes[0].Pubkey, layout.DefaultOptions())
	comm, k := cluster.Communities(g)
	return Input{Graph: g, Pos: pos, Communities: comm, K: k,
		Ours:    Ours{Pubkey: g.Nodes[0].Pubkey, URIs: []string{"1.2.3.4:9735"}},
		Version: version, Now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func read(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
	// The gzip copy says the same
	f, err := os.Open(path + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(z)
	if string(unzipped) != string(b) {
		t.Fatalf("%s.gz differs", path)
	}
}

func TestASmallGraphIsOneOverview(t *testing.T) {
	dir := t.TempDir()
	g := synthetic.Graph(40, 3, 2)
	g.Nodes[5].Alias = "Paperclip Pool"
	meta, err := Write(dir, input(g, 1))
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	read(t, filepath.Join(dir, "meta.json"), &m)
	if m.Version != 1 || m.Path != "v1/" || m.Nodes != 40 || m.Channels != len(g.Edges) ||
		len(m.Cells) != 0 || m.Ours.Pubkey != g.Nodes[0].Pubkey || m.Version != meta.Version {
		t.Fatalf("%+v", m)
	}
	var overview tile
	read(t, filepath.Join(dir, "v1", "overview.json"), &overview)
	if len(overview.Refs.IDs) != 0 || len(overview.Nodes.IDs) != 40 ||
		len(overview.Nodes.Pos) != 120 || len(overview.Edges.A) != len(g.Edges) ||
		overview.Aggregates != nil {
		t.Fatalf("refs %d ids %d edges %d", len(overview.Refs.IDs), len(overview.Nodes.IDs), len(overview.Edges.A))
	}
	// Ours at the origin
	i := 0
	for ; overview.Nodes.IDs[i] != m.Ours.Pubkey; i++ {
	}
	if overview.Nodes.Pos[3*i] != 0 || overview.Nodes.Pos[3*i+1] != 0 {
		t.Fatal("ours not at the origin")
	}
	var shard map[string]nodeInfo
	key := g.Nodes[5].Pubkey
	read(t, filepath.Join(dir, "v1", "node", Shard(key)+".json"), &shard)
	if shard[key].Alias != "Paperclip Pool" || len(shard[key].Channels) != g.Nodes[5].Degree {
		t.Fatalf("%+v", shard[key])
	}
	var search [][3]any
	read(t, filepath.Join(dir, "v1", "search", "pa.json"), &search)
	if len(search) != 1 || search[0][1] != key {
		t.Fatalf("%v", search)
	}
}

func TestVersionsAreKeptThreeDeep(t *testing.T) {
	dir := t.TempDir()
	g := synthetic.Graph(10, 2, 3)
	for v := 1; v <= 5; v++ {
		if _, err := Write(dir, input(g, v)); err != nil {
			t.Fatal(err)
		}
	}
	for v, want := range map[int]bool{1: false, 2: false, 3: true, 4: true, 5: true} {
		_, err := os.Stat(filepath.Join(dir, fmt.Sprintf("v%d", v)))
		if (err == nil) != want {
			t.Errorf("v%d kept: %v", v, err == nil)
		}
	}
	m, ok := ReadMeta(dir)
	if !ok || m.Version != 5 {
		t.Fatal(m)
	}
	later := m.GeneratedAt.Add(time.Hour)
	if err := Touch(dir, m, later); err != nil {
		t.Fatal(err)
	}
	if m2, _ := ReadMeta(dir); !m2.CheckedAt.Equal(later) || m2.Version != 5 {
		t.Fatal(m2)
	}
}

func TestPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"Paperclip": "pa", "a": "a_", "⚡zap": "_z", "42x": "42", "": "__", "A-B": "a_",
	} {
		if got := Prefix(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// dirSize counts the JSON a browser gets (not the gzip copies)
func dirSize(t *testing.T, dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".json" {
			total += info.Size()
		}
		return nil
	})
	return total
}

// GRAPH_BIG=1: 100,000 nodes in cells of at most 2,000, the overview at
// 2,000 with an aggregate per community, all files under 50 MB
func TestALargeGraphIsTiled(t *testing.T) {
	if os.Getenv("GRAPH_BIG") == "" {
		t.Skip("GRAPH_BIG not set")
	}
	dir := t.TempDir()
	g := synthetic.Graph(100_000, 3, 1)
	in := input(g, 1)
	start := time.Now()
	meta, err := Write(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	var overview tile
	read(t, filepath.Join(dir, "v1", "overview.json"), &overview)
	if len(overview.Nodes.IDs) != OverviewMax || overview.Aggregates == nil ||
		len(overview.Aggregates.Count) == 0 {
		t.Fatalf("overview %d members", len(overview.Nodes.IDs))
	}
	total := 0
	for _, c := range meta.Cells {
		if c.Nodes > CellMax {
			t.Fatalf("cell %s has %d", c.ID, c.Nodes)
		}
		total += c.Nodes
	}
	if total != 100_000 {
		t.Fatalf("cells hold %d nodes", total)
	}
	channels := 0
	for _, c := range meta.Cells {
		var cell tile
		read(t, filepath.Join(dir, "v1", c.File), &cell)
		channels += len(cell.Edges.A)
		n := len(cell.Nodes.IDs) + len(cell.Refs.IDs)
		for i := range cell.Edges.A {
			if cell.Edges.A[i] >= len(cell.Nodes.IDs) || cell.Edges.B[i] >= n {
				t.Fatalf("cell %s: channel %d points outside", c.ID, i)
			}
		}
	}
	if channels != len(g.Edges) {
		t.Fatalf("cells hold %d channels of %d", channels, len(g.Edges))
	}
	for _, part := range []string{"overview.json", "cells", "node", "search"} {
		t.Logf("%s: %d MB", part, dirSize(t, filepath.Join(dir, "v1", part))>>20)
	}
	size := dirSize(t, dir)
	// What a browser loads is small: no file over 2 MB (the overview,
	// a cell, a shard); the whole set is what the host keeps (x3 versions)
	var largest int64
	var largestName string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".json" && info.Size() > largest {
			largest, largestName = info.Size(), path
		}
		return nil
	})
	t.Logf("%d cells, %d MB in all, the largest file %d KB (%s), written in %v",
		len(meta.Cells), size>>20, largest>>10, filepath.Base(largestName), took)
	if largest > 2<<20 || size > 150<<20 {
		t.Fatalf("largest %d KB, all %d MB", largest>>10, size>>20)
	}
}
