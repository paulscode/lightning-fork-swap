package layout

import (
	"os"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/graph/internal/synthetic"
)

// GRAPH_BIG=1: the budget of the plan, a 100,000-node graph laid out in
// under a minute, and again from its positions quicker still
func TestBudgetOn100kNodes(t *testing.T) {
	if os.Getenv("GRAPH_BIG") == "" {
		t.Skip("GRAPH_BIG not set")
	}
	g := synthetic.Graph(100_000, 3, 1)
	start := time.Now()
	pos := Run(g, nil, g.Nodes[0].Pubkey, DefaultOptions())
	cold := time.Since(start)
	prev := map[string]Vec{}
	for i, n := range g.Nodes {
		prev[n.Pubkey] = pos[i]
	}
	start = time.Now()
	Run(g, prev, g.Nodes[0].Pubkey, DefaultOptions())
	warm := time.Since(start)
	t.Logf("%d nodes, %d channels: cold %v, warm %v", len(g.Nodes), len(g.Edges), cold, warm)
	if cold > time.Minute || warm > 30*time.Second {
		t.Fatalf("over budget")
	}
}
