package cluster

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
	"github.com/paulscode/lightning-fork-swap/graph/internal/synthetic"
)

func groups(k, n int) *model.Graph {
	var nodes []model.Node
	var edges []model.Edge
	for i := 0; i < k*n; i++ {
		nodes = append(nodes, model.Node{Pubkey: fmt.Sprintf("02%064x", i)})
	}
	for g := 0; g < k; g++ {
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				edges = append(edges, model.Edge{ID: fmt.Sprintf("%d-%d", g*n+i, g*n+j),
					A: g*n + i, B: g*n + j, Capacity: 1_000_000})
			}
		}
		// A ring of bridges between groups
		edges = append(edges, model.Edge{ID: fmt.Sprintf("b%d", g), A: g * n,
			B: ((g + 1) % k) * n, Capacity: 1_000_000})
	}
	return model.New(nodes, edges)
}

func TestFindsTheGroups(t *testing.T) {
	g := groups(4, 6)
	assign, k := Communities(g)
	if k != 4 {
		t.Fatalf("%d communities: %v", k, assign)
	}
	for grp := 0; grp < 4; grp++ {
		for i := 1; i < 6; i++ {
			if assign[grp*6+i] != assign[grp*6] {
				t.Fatalf("group %d split: %v", grp, assign)
			}
		}
	}
	if q := Modularity(g, assign); q < 0.6 {
		t.Fatalf("modularity %.2f", q)
	}
	again, _ := Communities(g)
	for i := range assign {
		if again[i] != assign[i] {
			t.Fatal("not deterministic")
		}
	}
}

func TestDegenerateGraphs(t *testing.T) {
	if assign, k := Communities(model.New(nil, nil)); len(assign) != 0 || k != 0 {
		t.Fatal(assign, k)
	}
	lonely := model.New([]model.Node{{Pubkey: "a"}, {Pubkey: "b"}}, nil)
	if assign, k := Communities(lonely); k != 2 || assign[0] == assign[1] {
		t.Fatal(assign, k)
	}
}

func TestLarge(t *testing.T) {
	if os.Getenv("GRAPH_BIG") == "" {
		t.Skip("GRAPH_BIG not set")
	}
	g := synthetic.Graph(100_000, 3, 1)
	start := time.Now()
	assign, k := Communities(g)
	t.Logf("%d communities in %v, modularity %.2f", k, time.Since(start), Modularity(g, assign))
}
