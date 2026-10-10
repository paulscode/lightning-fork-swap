// Package model is the channel graph as the generator sees it: nodes and
// the announced channels between them, from lnd's DescribeGraph.
package model

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Node is one node of the graph.
type Node struct {
	Pubkey     string   `json:"pubkey"`
	Alias      string   `json:"alias"`
	Color      string   `json:"color"`
	Addresses  []string `json:"addresses"`
	LastUpdate int64    `json:"lastUpdate"`
	// Derived
	Capacity int64 `json:"capacity"`
	Degree   int   `json:"degree"`
}

// Edge is one announced channel.
type Edge struct {
	ID       string `json:"id"`
	A, B     int    `json:"-"` // node indexes
	Capacity int64  `json:"capacity"`
	// A stable number from the channel id, for its curve
	Seed uint32 `json:"seed"`
}

// Graph is a snapshot: nodes sorted by pubkey, edges by id.
type Graph struct {
	Nodes []Node
	Edges []Edge
	index map[string]int
}

// Index is a node's position in Nodes, or -1.
func (g *Graph) Index(pubkey string) int {
	if i, ok := g.index[pubkey]; ok {
		return i
	}
	return -1
}

// lnd's JSON (its REST API sends 64-bit numbers as strings)
type describeGraph struct {
	Nodes []struct {
		PubKey     string `json:"pub_key"`
		Alias      string `json:"alias"`
		Color      string `json:"color"`
		LastUpdate int64  `json:"last_update"`
		Addresses  []struct {
			Addr string `json:"addr"`
		} `json:"addresses"`
	} `json:"nodes"`
	Edges []struct {
		ChannelID string          `json:"channel_id"`
		Node1     string          `json:"node1_pub"`
		Node2     string          `json:"node2_pub"`
		Capacity  json.RawMessage `json:"capacity"`
	} `json:"edges"`
}

func number(raw json.RawMessage) int64 {
	v, _ := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	return v
}

var pubkeyChars = func(s string) bool {
	if len(s) != 66 || (s[:2] != "02" && s[:2] != "03") {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Clean keeps an alias to printable text of at most 32 bytes (what lnd
// allows); anything else could break the page that shows it.
func Clean(alias string) string {
	var b strings.Builder
	for _, r := range alias {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0xfffd {
			continue
		}
		if b.Len()+len(string(r)) > 32 {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func color(c string) string {
	if len(c) == 7 && c[0] == '#' {
		if _, err := strconv.ParseUint(c[1:], 16, 32); err == nil {
			return strings.ToLower(c)
		}
	}
	return "#3399ff"
}

// Parse reads DescribeGraph's answer. Nodes without a valid key, and
// channels to unknown nodes or with no capacity, are left out.
func Parse(b []byte) (*Graph, error) {
	var in describeGraph
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	g := &Graph{index: map[string]int{}}
	for _, n := range in.Nodes {
		if !pubkeyChars(n.PubKey) {
			continue
		}
		var addrs []string
		for _, a := range n.Addresses {
			if a.Addr != "" && len(a.Addr) < 100 {
				addrs = append(addrs, a.Addr)
			}
		}
		g.Nodes = append(g.Nodes, Node{Pubkey: n.PubKey, Alias: Clean(n.Alias),
			Color: color(n.Color), Addresses: addrs, LastUpdate: n.LastUpdate})
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Pubkey < g.Nodes[j].Pubkey })
	for i, n := range g.Nodes {
		g.index[n.Pubkey] = i
	}
	seen := map[string]bool{}
	for _, e := range in.Edges {
		a, b := g.Index(e.Node1), g.Index(e.Node2)
		capacity := number(e.Capacity)
		if a < 0 || b < 0 || a == b || capacity <= 0 || seen[e.ChannelID] {
			continue
		}
		seen[e.ChannelID] = true
		g.Edges = append(g.Edges, Edge{ID: e.ChannelID, A: a, B: b, Capacity: capacity,
			Seed: Seed(e.ChannelID)})
	}
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].ID < g.Edges[j].ID })
	for _, e := range g.Edges {
		g.Nodes[e.A].Capacity += e.Capacity
		g.Nodes[e.B].Capacity += e.Capacity
		g.Nodes[e.A].Degree++
		g.Nodes[e.B].Degree++
	}
	return g, nil
}

// Seed is a stable 32-bit number from a channel id (FNV-1a).
func Seed(id string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return h
}

// Importance orders nodes for level of detail: log capacity plus log
// degree; our node first of all.
func (g *Graph) Importance(ours string) []float64 {
	out := make([]float64, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = math.Log1p(float64(n.Capacity)) + 4*math.Log1p(float64(n.Degree))
		if n.Pubkey == ours {
			out[i] = math.Inf(1)
		}
	}
	return out
}

// Neighbours lists each node's neighbours.
func (g *Graph) Neighbours() [][]int {
	out := make([][]int, len(g.Nodes))
	for _, e := range g.Edges {
		out[e.A] = append(out[e.A], e.B)
		out[e.B] = append(out[e.B], e.A)
	}
	return out
}

// TotalCapacity of all channels.
func (g *Graph) TotalCapacity() int64 {
	var t int64
	for _, e := range g.Edges {
		t += e.Capacity
	}
	return t
}

// New builds a graph from nodes and edges (tests and synthetic graphs);
// edges refer to nodes by index in the given order.
func New(nodes []Node, edges []Edge) *Graph {
	g := &Graph{Nodes: nodes, Edges: edges, index: map[string]int{}}
	for i, n := range g.Nodes {
		g.index[n.Pubkey] = i
	}
	for i := range g.Edges {
		e := &g.Edges[i]
		if e.Seed == 0 {
			e.Seed = Seed(e.ID)
		}
		g.Nodes[e.A].Capacity += e.Capacity
		g.Nodes[e.B].Capacity += e.Capacity
		g.Nodes[e.A].Degree++
		g.Nodes[e.B].Degree++
	}
	return g
}
