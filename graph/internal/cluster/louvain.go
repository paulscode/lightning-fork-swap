// Package cluster finds communities in the graph (Louvain modularity), for
// the sky's regional hues and, on large graphs, the aggregate "galaxies"
// of the zoomed-out view. Deterministic: nodes are visited in index order.
package cluster

import (
	"math"
	"sort"

	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
)

type graph struct {
	n      int
	adj    []map[int]float64 // neighbour -> weight (self loops as [i][i])
	degree []float64         // weighted degree
	total  float64           // sum of all weights, both directions
}

func fromModel(g *model.Graph) *graph {
	out := &graph{n: len(g.Nodes), adj: make([]map[int]float64, len(g.Nodes)),
		degree: make([]float64, len(g.Nodes))}
	for i := range out.adj {
		out.adj[i] = map[int]float64{}
	}
	for _, e := range g.Edges {
		// Bigger channels bind a little harder; every channel counts
		w := 1 + math.Log1p(float64(e.Capacity)/1e6)
		out.adj[e.A][e.B] += w
		out.adj[e.B][e.A] += w
	}
	out.weigh()
	return out
}

func (g *graph) weigh() {
	g.total = 0
	for i := range g.adj {
		g.degree[i] = 0
		for j, w := range g.adj[i] {
			g.degree[i] += w
			if i == j {
				g.degree[i] += w // a self loop counts twice
			}
		}
		g.total += g.degree[i]
	}
}

func sortedKeys(m map[int]float64) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// onePass moves nodes between communities while modularity rises; it
// returns each node's community and whether anything moved.
func (g *graph) onePass() ([]int, bool) {
	comm := make([]int, g.n)
	tot := make([]float64, g.n) // weighted degree of each community
	for i := range comm {
		comm[i], tot[i] = i, g.degree[i]
	}
	if g.total == 0 {
		return comm, false
	}
	moved := false
	for round := 0; round < 50; round++ {
		changed := 0
		for i := 0; i < g.n; i++ {
			own := comm[i]
			links := map[int]float64{}
			for _, j := range sortedKeys(g.adj[i]) {
				if j != i {
					links[comm[j]] += g.adj[i][j]
				}
			}
			// The gain of joining c: k(i,c) - tot(c) k(i) / m, with m the
			// total edge weight (g.total counts each edge from both ends)
			tot[own] -= g.degree[i]
			m := g.total / 2
			best, bestGain := own, links[own]-tot[own]*g.degree[i]/m
			for _, c := range sortedKeys(links) {
				gain := links[c] - tot[c]*g.degree[i]/m
				if gain > bestGain+1e-12 {
					best, bestGain = c, gain
				}
			}
			tot[best] += g.degree[i]
			if best != own {
				comm[i] = best
				changed++
				moved = true
			}
		}
		if changed == 0 {
			break
		}
	}
	return renumber(comm), moved
}

// renumber makes community ids 0..k-1 in order of first appearance.
func renumber(comm []int) []int {
	ids := map[int]int{}
	out := make([]int, len(comm))
	for i, c := range comm {
		id, ok := ids[c]
		if !ok {
			id = len(ids)
			ids[c] = id
		}
		out[i] = id
	}
	return out
}

func (g *graph) aggregate(comm []int) *graph {
	k := 0
	for _, c := range comm {
		if c+1 > k {
			k = c + 1
		}
	}
	out := &graph{n: k, adj: make([]map[int]float64, k), degree: make([]float64, k)}
	for i := range out.adj {
		out.adj[i] = map[int]float64{}
	}
	for i := range g.adj {
		for j, w := range g.adj[i] {
			out.adj[comm[i]][comm[j]] += w
		}
	}
	// Each undirected edge was added from both ends; self loops once each
	for c := range out.adj {
		if w, ok := out.adj[c][c]; ok {
			out.adj[c][c] = w / 2
		}
	}
	out.weigh()
	return out
}

// Communities gives each node a community id (0..k-1, the largest
// community first) and the number of communities.
func Communities(m *model.Graph) ([]int, int) {
	g := fromModel(m)
	assign := make([]int, g.n)
	for i := range assign {
		assign[i] = i
	}
	for level := 0; level < 10; level++ {
		comm, moved := g.onePass()
		for i := range assign {
			assign[i] = comm[assign[i]]
		}
		if !moved {
			break
		}
		g = g.aggregate(comm)
	}
	// Largest first, ties by first member
	size := map[int]int{}
	first := map[int]int{}
	for i, c := range assign {
		size[c]++
		if _, ok := first[c]; !ok {
			first[c] = i
		}
	}
	order := make([]int, 0, len(size))
	for c := range size {
		order = append(order, c)
	}
	sort.Slice(order, func(a, b int) bool {
		if size[order[a]] != size[order[b]] {
			return size[order[a]] > size[order[b]]
		}
		return first[order[a]] < first[order[b]]
	})
	rank := map[int]int{}
	for r, c := range order {
		rank[c] = r
	}
	for i := range assign {
		assign[i] = rank[assign[i]]
	}
	return assign, len(order)
}

// Modularity of an assignment (for tests).
func Modularity(m *model.Graph, assign []int) float64 {
	g := fromModel(m)
	if g.total == 0 {
		return 0
	}
	in := map[int]float64{}
	tot := map[int]float64{}
	for i := range g.adj {
		tot[assign[i]] += g.degree[i]
		for j, w := range g.adj[i] {
			if assign[i] == assign[j] {
				in[assign[i]] += w
			}
		}
	}
	q := 0.0
	for c, t := range tot {
		q += in[c]/g.total - (t/g.total)*(t/g.total)
	}
	return q
}
