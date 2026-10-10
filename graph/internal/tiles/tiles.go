// Package tiles writes the sky's files: a meta.json naming the current
// version, and under v<N>/ the overview (the whole graph while it is
// small; the most important nodes and an aggregate star per community
// once it is not), octree cells for the rest, each node's full adjacency
// in shards, and search shards. Every file has a gzip copy for nginx. A
// version is written whole before meta.json points at it; the last three
// are kept.
package tiles

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/paulscode/lightning-fork-swap/graph/internal/layout"
	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
)

// Limits of one file
const (
	OverviewMax = 2000 // nodes in the overview
	CellMax     = 2000 // nodes in an octree cell
	Keep        = 3    // versions kept
)

// Ours is the swap service's node.
type Ours struct {
	Pubkey string   `json:"pubkey"`
	URIs   []string `json:"uris"`
}

// Meta is meta.json.
type Meta struct {
	Version     int           `json:"version"`
	GeneratedAt time.Time     `json:"generatedAt"`
	CheckedAt   time.Time     `json:"checkedAt"`
	Nodes       int           `json:"nodes"`
	Channels    int           `json:"channels"`
	Capacity    int64         `json:"capacity"`
	Communities int           `json:"communities"`
	Bounds      [2][3]float64 `json:"bounds"`
	Ours        Ours          `json:"ours"`
	Path        string        `json:"path"`
	Overview    string        `json:"overview"`
	Cells       []Cell        `json:"cells"`
	NodeShards  string        `json:"nodeShards"`
	Search      string        `json:"search"`
}

// Cell is one octree cell's file.
type Cell struct {
	ID    string     `json:"id"`
	Level int        `json:"level"`
	Lo    [3]float64 `json:"lo"`
	Hi    [3]float64 `json:"hi"`
	Nodes int        `json:"nodes"`
	File  string     `json:"file"`
}

// Columns of a file's nodes. Channels index them, and past them its
// refs: the far ends of channels that leave the file (their key and
// position only), so every channel can be drawn.
type nodeColumns struct {
	IDs   []string  `json:"ids"`
	Alias []string  `json:"alias"`
	Pos   []float64 `json:"pos"`
	Cap   []int64   `json:"cap"`
	Deg   []int     `json:"deg"`
	Comm  []int     `json:"comm"`
	Color []string  `json:"color"`
}

type edgeColumns struct {
	A    []int    `json:"a"`
	B    []int    `json:"b"`
	Cap  []int64  `json:"cap"`
	Seed []uint32 `json:"seed"`
}

type aggregates struct {
	Pos   []float64 `json:"pos"`
	Cap   []int64   `json:"cap"`
	Count []int     `json:"count"`
	Comm  []int     `json:"comm"`
}

type refColumns struct {
	IDs []string  `json:"ids"`
	Pos []float64 `json:"pos"`
}

type tile struct {
	Nodes      nodeColumns `json:"nodes"`
	Refs       refColumns  `json:"refs"`
	Edges      edgeColumns `json:"edges"`
	Aggregates *aggregates `json:"aggregates,omitempty"`
}

// Input is everything one version is made of.
type Input struct {
	Graph       *model.Graph
	Pos         []layout.Vec
	Communities []int
	K           int
	Ours        Ours
	Version     int
	Now         time.Time
}

func round(v float64) float64 { return math.Round(v*1000) / 1000 }

// fileTile builds a tile of these members (indexes into the graph) and
// the channels edgesFor picks (at least one end among them).
func fileTile(in Input, members []int, edgesFor func(e model.Edge) bool) tile {
	g := in.Graph
	var t tile
	local := map[int]int{}
	for _, i := range members {
		local[i] = len(t.Nodes.IDs)
		n := g.Nodes[i]
		t.Nodes.IDs = append(t.Nodes.IDs, n.Pubkey)
		t.Nodes.Alias = append(t.Nodes.Alias, n.Alias)
		p := in.Pos[i]
		t.Nodes.Pos = append(t.Nodes.Pos, round(p[0]), round(p[1]), round(p[2]))
		t.Nodes.Cap = append(t.Nodes.Cap, n.Capacity)
		t.Nodes.Deg = append(t.Nodes.Deg, n.Degree)
		t.Nodes.Comm = append(t.Nodes.Comm, in.Communities[i])
		t.Nodes.Color = append(t.Nodes.Color, n.Color)
	}
	ref := func(i int) int {
		if l, ok := local[i]; ok {
			return l
		}
		l := len(t.Nodes.IDs) + len(t.Refs.IDs)
		local[i] = l
		p := in.Pos[i]
		t.Refs.IDs = append(t.Refs.IDs, g.Nodes[i].Pubkey)
		t.Refs.Pos = append(t.Refs.Pos, round(p[0]), round(p[1]), round(p[2]))
		return l
	}
	for _, e := range g.Edges {
		if !edgesFor(e) {
			continue
		}
		t.Edges.A = append(t.Edges.A, ref(e.A))
		t.Edges.B = append(t.Edges.B, ref(e.B))
		t.Edges.Cap = append(t.Edges.Cap, e.Capacity)
		t.Edges.Seed = append(t.Edges.Seed, e.Seed)
	}
	if t.Refs.IDs == nil {
		t.Refs = refColumns{IDs: []string{}, Pos: []float64{}}
	}
	return t
}

// Write makes version in.Version under dir, then points meta.json at it.
func Write(dir string, in Input) (Meta, error) {
	g := in.Graph
	vdir := fmt.Sprintf("v%d", in.Version)
	root := filepath.Join(dir, vdir)
	if err := os.RemoveAll(root); err != nil {
		return Meta{}, err
	}
	meta := Meta{Version: in.Version, GeneratedAt: in.Now, CheckedAt: in.Now,
		Nodes: len(g.Nodes), Channels: len(g.Edges), Capacity: g.TotalCapacity(),
		Communities: in.K, Ours: in.Ours, Path: vdir + "/", Overview: "overview.json",
		Cells: []Cell{}, NodeShards: "node/{shard}.json", Search: "search/{prefix}.json"}
	for k := 0; k < 3; k++ {
		meta.Bounds[0][k], meta.Bounds[1][k] = math.Inf(1), math.Inf(-1)
	}
	for _, p := range in.Pos {
		for k := 0; k < 3; k++ {
			meta.Bounds[0][k] = math.Min(meta.Bounds[0][k], round(p[k]))
			meta.Bounds[1][k] = math.Max(meta.Bounds[1][k], round(p[k]))
		}
	}
	if len(in.Pos) == 0 {
		meta.Bounds = [2][3]float64{}
	}

	// The overview: everything, or the most important and our
	// neighbourhood, with one aggregate per community for the rest
	importance := g.Importance(in.Ours.Pubkey)
	order := make([]int, len(g.Nodes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return importance[order[a]] > importance[order[b]] })
	inOverview := make([]bool, len(g.Nodes))
	var members []int
	if ours := g.Index(in.Ours.Pubkey); ours >= 0 {
		inOverview[ours] = true
		members = append(members, ours)
		for _, j := range g.Neighbours()[ours] {
			if !inOverview[j] {
				inOverview[j] = true
				members = append(members, j)
			}
		}
	}
	for _, i := range order {
		if len(members) >= OverviewMax {
			break
		}
		if !inOverview[i] {
			inOverview[i] = true
			members = append(members, i)
		}
	}
	sort.Ints(members)
	overview := fileTile(in, members, func(e model.Edge) bool { return inOverview[e.A] && inOverview[e.B] })
	if len(members) < len(g.Nodes) {
		agg := &aggregates{}
		sums := map[int]*struct {
			pos   layout.Vec
			cap   int64
			count int
		}{}
		for i, c := range in.Communities {
			if inOverview[i] {
				continue
			}
			s, ok := sums[c]
			if !ok {
				s = &struct {
					pos   layout.Vec
					cap   int64
					count int
				}{}
				sums[c] = s
			}
			for k := 0; k < 3; k++ {
				s.pos[k] += in.Pos[i][k]
			}
			s.cap += g.Nodes[i].Capacity
			s.count++
		}
		comms := make([]int, 0, len(sums))
		for c := range sums {
			comms = append(comms, c)
		}
		sort.Ints(comms)
		for _, c := range comms {
			s := sums[c]
			agg.Pos = append(agg.Pos, round(s.pos[0]/float64(s.count)),
				round(s.pos[1]/float64(s.count)), round(s.pos[2]/float64(s.count)))
			agg.Cap = append(agg.Cap, s.cap)
			agg.Count = append(agg.Count, s.count)
			agg.Comm = append(agg.Comm, c)
		}
		overview.Aggregates = agg
	}
	if err := writeJSON(root, "overview.json", overview); err != nil {
		return Meta{}, err
	}

	// Octree cells, once the graph outgrows the overview
	if len(g.Nodes) > OverviewMax {
		all := make([]int, len(g.Nodes))
		for i := range all {
			all[i] = i
		}
		cellOf := make([]string, len(g.Nodes))
		cells, err := writeCells(root, in, all, meta.Bounds[0], meta.Bounds[1], 1, "c", cellOf)
		if err != nil {
			return Meta{}, err
		}
		meta.Cells = cells
	}

	if err := writeNodeShards(root, in); err != nil {
		return Meta{}, err
	}
	if err := writeSearch(root, in); err != nil {
		return Meta{}, err
	}
	// The version is whole: point meta.json at it, then forget old ones
	if err := writeJSON(dir, "meta.json", meta); err != nil {
		return Meta{}, err
	}
	return meta, prune(dir, in.Version)
}

// writeCells splits members into octants until each holds at most
// CellMax nodes.
// Each channel is in one cell, its first end's: a cell's filaments fill
// in as the cells around it load (a selected node's come whole from its
// shard).
func writeCells(root string, in Input, members []int, lo, hi [3]float64, level int, id string, cellOf []string) ([]Cell, error) {
	if len(members) <= CellMax || level > 12 {
		set := map[int]bool{}
		for _, i := range members {
			set[i] = true
			cellOf[i] = id
		}
		t := fileTile(in, members, func(e model.Edge) bool { return set[e.A] })
		file := "cells/" + id + ".json"
		if err := writeJSON(root, file, t); err != nil {
			return nil, err
		}
		return []Cell{{ID: id, Level: level, Lo: lo, Hi: hi, Nodes: len(members), File: file}}, nil
	}
	var mid [3]float64
	for k := 0; k < 3; k++ {
		mid[k] = (lo[k] + hi[k]) / 2
	}
	parts := make([][]int, 8)
	for _, i := range members {
		o := 0
		for k := 0; k < 3; k++ {
			if in.Pos[i][k] >= mid[k] {
				o |= 1 << k
			}
		}
		parts[o] = append(parts[o], i)
	}
	var out []Cell
	for o, part := range parts {
		if len(part) == 0 {
			continue
		}
		var clo, chi [3]float64
		for k := 0; k < 3; k++ {
			if o&(1<<k) != 0 {
				clo[k], chi[k] = mid[k], hi[k]
			} else {
				clo[k], chi[k] = lo[k], mid[k]
			}
		}
		cells, err := writeCells(root, in, part, clo, chi, level+1, id+strconv.Itoa(o), cellOf)
		if err != nil {
			return nil, err
		}
		out = append(out, cells...)
	}
	return out, nil
}

// Shard is where a node's adjacency is: the two hex digits after 02/03.
func Shard(pubkey string) string { return pubkey[2:4] }

type nodeInfo struct {
	Alias      string     `json:"alias"`
	Color      string     `json:"color"`
	Addresses  []string   `json:"addresses"`
	Capacity   int64      `json:"capacity"`
	Degree     int        `json:"degree"`
	LastUpdate int64      `json:"lastUpdate"`
	Comm       int        `json:"comm"`
	Pos        [3]float64 `json:"pos"`
	// Each channel: the other end and its capacity
	Channels [][2]any `json:"channels"`
}

func writeNodeShards(root string, in Input) error {
	g := in.Graph
	shards := map[string]map[string]*nodeInfo{}
	for i, n := range g.Nodes {
		s := Shard(n.Pubkey)
		if shards[s] == nil {
			shards[s] = map[string]*nodeInfo{}
		}
		p := in.Pos[i]
		shards[s][n.Pubkey] = &nodeInfo{Alias: n.Alias, Color: n.Color, Addresses: n.Addresses,
			Capacity: n.Capacity, Degree: n.Degree, LastUpdate: n.LastUpdate, Comm: in.Communities[i],
			Pos: [3]float64{round(p[0]), round(p[1]), round(p[2])}, Channels: [][2]any{}}
	}
	for _, e := range g.Edges {
		a, b := g.Nodes[e.A].Pubkey, g.Nodes[e.B].Pubkey
		shards[Shard(a)][a].Channels = append(shards[Shard(a)][a].Channels, [2]any{b, e.Capacity})
		shards[Shard(b)][b].Channels = append(shards[Shard(b)][b].Channels, [2]any{a, e.Capacity})
	}
	for s, nodes := range shards {
		if err := writeJSON(root, "node/"+s+".json", nodes); err != nil {
			return err
		}
	}
	return nil
}

// Prefix is the search shard of an alias: its first two letters or
// digits, lowercased ("_" for anything else).
func Prefix(alias string) string {
	var b []rune
	for _, r := range strings.ToLower(alias) {
		if len(b) == 2 {
			break
		}
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b = append(b, r)
		} else {
			b = append(b, '_')
		}
	}
	for len(b) < 2 {
		b = append(b, '_')
	}
	return string(b)
}

func writeSearch(root string, in Input) error {
	shards := map[string][][3]any{}
	for _, n := range in.Graph.Nodes {
		if n.Alias == "" {
			continue
		}
		p := Prefix(n.Alias)
		shards[p] = append(shards[p], [3]any{n.Alias, n.Pubkey, n.Capacity})
	}
	for p, entries := range shards {
		if err := writeJSON(root, "search/"+p+".json", entries); err != nil {
			return err
		}
	}
	return nil
}

// writeJSON writes name (and name.gz) under dir, atomically.
func writeJSON(dir, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var z bytes.Buffer
	w, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
	_, _ = w.Write(b)
	_ = w.Close()
	for _, f := range []struct {
		path string
		data []byte
	}{{path, b}, {path + ".gz", z.Bytes()}} {
		tmp := f.path + ".tmp"
		if err := os.WriteFile(tmp, f.data, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return err
		}
	}
	return nil
}

// prune removes versions older than the last Keep.
func prune(dir string, current int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "v") {
			continue
		}
		v, err := strconv.Atoi(e.Name()[1:])
		if err != nil || v > current-Keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Touch rewrites meta.json with a new CheckedAt, when nothing changed.
func Touch(dir string, meta Meta, now time.Time) error {
	meta.CheckedAt = now
	return writeJSON(dir, "meta.json", meta)
}

// ReadMeta reads the current meta.json, if any.
func ReadMeta(dir string) (Meta, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return Meta{}, false
	}
	var m Meta
	if json.Unmarshal(b, &m) != nil {
		return Meta{}, false
	}
	return m, true
}
