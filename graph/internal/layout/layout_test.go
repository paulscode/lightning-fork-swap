package layout

import (
	"fmt"
	"math"
	"testing"

	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
)

func key(i int) string { return fmt.Sprintf("02%064x", i) }

// Two groups of n nodes, every node linked inside its group, one bridge
func twoGroups(n int) *model.Graph {
	var nodes []model.Node
	var edges []model.Edge
	for i := 0; i < 2*n; i++ {
		nodes = append(nodes, model.Node{Pubkey: key(i)})
	}
	for g := 0; g < 2; g++ {
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				edges = append(edges, model.Edge{ID: fmt.Sprintf("%d-%d", g*n+i, g*n+j),
					A: g*n + i, B: g*n + j, Capacity: 1_000_000})
			}
		}
	}
	edges = append(edges, model.Edge{ID: "bridge", A: 0, B: n, Capacity: 1_000_000})
	return model.New(nodes, edges)
}

func dist(a, b Vec) float64 { return a.sub(b).len() }

func TestDeterministicPinnedAndShaped(t *testing.T) {
	g := twoGroups(8)
	a := Run(g, nil, key(0), DefaultOptions())
	b := Run(g, nil, key(0), DefaultOptions())
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("node %d: %v then %v", i, a[i], b[i])
		}
		for k := 0; k < 3; k++ {
			if math.IsNaN(a[i][k]) || math.IsInf(a[i][k], 0) {
				t.Fatalf("node %d at %v", i, a[i])
			}
		}
	}
	if a[0] != (Vec{}) {
		t.Fatalf("ours at %v", a[0])
	}
	var inside, across float64
	var ni, na int
	for i := 0; i < 16; i++ {
		for j := i + 1; j < 16; j++ {
			if (i < 8) == (j < 8) {
				inside += dist(a[i], a[j])
				ni++
			} else {
				across += dist(a[i], a[j])
				na++
			}
		}
	}
	if inside/float64(ni) >= across/float64(na)*0.8 {
		t.Fatalf("groups not apart: inside %.2f, across %.2f", inside/float64(ni), across/float64(na))
	}
}

func TestAWarmStartKeepsTheShape(t *testing.T) {
	g := twoGroups(8)
	first := Run(g, nil, key(0), DefaultOptions())
	prev := map[string]Vec{}
	for i, n := range g.Nodes {
		prev[n.Pubkey] = first[i]
	}
	// One new node with a channel to node 9
	nodes := append([]model.Node(nil), g.Nodes...)
	for i := range nodes {
		nodes[i].Capacity, nodes[i].Degree = 0, 0
	}
	nodes = append(nodes, model.Node{Pubkey: key(99)})
	edges := append([]model.Edge(nil), g.Edges...)
	edges = append(edges, model.Edge{ID: "new", A: 9, B: 16, Capacity: 1_000_000})
	g2 := model.New(nodes, edges)
	second := Run(g2, prev, key(0), DefaultOptions())
	scale := 0.0
	for i := 0; i < 16; i++ {
		scale += first[i].len()
	}
	scale /= 16
	moved := 0.0
	for i := 0; i < 16; i++ {
		moved += dist(first[i], second[i])
	}
	moved /= 16
	if moved > scale*0.25 {
		t.Fatalf("the old nodes moved %.2f on average (the sky is %.2f across)", moved, scale)
	}
	// The newcomer lands near its neighbour, not anywhere
	if dist(second[16], second[9]) > dist(second[16], second[0])*1.5 &&
		dist(second[16], second[9]) > scale {
		t.Fatalf("new node %.2f from its neighbour", dist(second[16], second[9]))
	}
}

func TestEmptyAndSingle(t *testing.T) {
	if got := Run(model.New(nil, nil), nil, "", DefaultOptions()); len(got) != 0 {
		t.Fatal(got)
	}
	got := Run(model.New([]model.Node{{Pubkey: key(1)}}, nil), nil, key(1), DefaultOptions())
	if got[0] != (Vec{}) {
		t.Fatal(got)
	}
	// Two nodes in the same place are pushed apart, without NaN
	g := model.New([]model.Node{{Pubkey: key(1)}, {Pubkey: key(2)}},
		[]model.Edge{{ID: "x", A: 0, B: 1, Capacity: 5}})
	prev := map[string]Vec{key(1): {1, 1, 1}, key(2): {1, 1, 1}}
	got = Run(g, prev, "", DefaultOptions())
	if dist(got[0], got[1]) < 0.1 || math.IsNaN(got[0][0]) {
		t.Fatalf("%v", got)
	}
}
