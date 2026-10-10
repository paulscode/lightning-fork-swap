// Package synthetic makes large made-up graphs for the generator's size
// and time budgets: preferential attachment, like real Lightning graphs
// (a few hubs, many small nodes).
package synthetic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"

	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
)

// Graph of n nodes, each new one opening m channels to earlier nodes
// chosen in proportion to their degree.
func Graph(n, m int, seed int64) *model.Graph {
	rng := rand.New(rand.NewSource(seed))
	nodes := make([]model.Node, n)
	for i := range nodes {
		// Keys spread like real ones (they shard by their first bytes)
		h := sha256.Sum256([]byte(fmt.Sprint(seed, i)))
		// Aliases starting all over the alphabet, as real ones do
		alias := fmt.Sprintf("%c%c-node-%d", 'a'+h[0]%26, 'a'+h[1]%26, i)
		nodes[i] = model.Node{Pubkey: "02" + hex.EncodeToString(h[:]), Alias: alias,
			Color: "#3399ff"}
	}
	var edges []model.Edge
	var targets []int // each node once per channel end
	for i := 1; i < n; i++ {
		seen := map[int]bool{}
		for k := 0; k < m && k < i; k++ {
			var j int
			if len(targets) == 0 || rng.Intn(4) == 0 {
				j = rng.Intn(i)
			} else {
				j = targets[rng.Intn(len(targets))]
			}
			if seen[j] {
				continue
			}
			seen[j] = true
			capacity := int64(1_000_000 * (1 + rng.Intn(50)))
			edges = append(edges, model.Edge{ID: fmt.Sprintf("%d:%d", i, j), A: i, B: j, Capacity: capacity})
			targets = append(targets, i, j)
		}
	}
	return model.New(nodes, edges)
}
