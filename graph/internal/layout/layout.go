// Package layout places the graph's nodes in 3D: force-directed (springs
// along channels, repulsion between all nodes through a Barnes-Hut
// octree), started from the previous positions so the sky keeps its shape
// from one snapshot to the next, with our node fixed at the origin.
// Deterministic: the same graph and previous positions give the same
// result.
package layout

import (
	"fmt"
	"hash/fnv"
	"math"
	"runtime"
	"sync"

	"github.com/paulscode/lightning-fork-swap/graph/internal/model"
)

// Vec is a point in space.
type Vec [3]float64

func (a Vec) add(b Vec) Vec       { return Vec{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func (a Vec) sub(b Vec) Vec       { return Vec{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func (a Vec) scale(s float64) Vec { return Vec{a[0] * s, a[1] * s, a[2] * s} }
func (a Vec) len() float64        { return math.Sqrt(a[0]*a[0] + a[1]*a[1] + a[2]*a[2]) }

// Options tune a run.
type Options struct {
	// Steps of a run from nothing, and of a run from previous positions
	ColdSteps, WarmSteps int
	// Barnes-Hut accuracy: a cell this wide relative to its distance
	// counts as one body
	Theta float64
	// Pull toward the origin (keeps separate parts of the graph close)
	Gravity float64
}

// DefaultOptions suit graphs from tens to hundreds of thousands of nodes.
func DefaultOptions() Options {
	return Options{ColdSteps: 300, WarmSteps: 60, Theta: 0.9, Gravity: 0.02}
}

// hashUnit is a stable number in [-1, 1) from a string and a salt.
func hashUnit(s string, salt byte) float64 {
	h := fnv.New64a()
	h.Write([]byte{salt})
	h.Write([]byte(s))
	return float64(h.Sum64()>>11)/float64(1<<52) - 1
}

func jitter(pubkey string, r float64) Vec {
	return Vec{hashUnit(pubkey, 1) * r, hashUnit(pubkey, 2) * r, hashUnit(pubkey, 3) * r}
}

// Run lays the graph out. prev maps pubkeys to earlier positions; ours is
// pinned at the origin (may be "").
func Run(g *model.Graph, prev map[string]Vec, ours string, o Options) []Vec {
	n := len(g.Nodes)
	pos := make([]Vec, n)
	placed := make([]bool, n)
	known := 0
	for i, node := range g.Nodes {
		if p, ok := prev[node.Pubkey]; ok {
			pos[i], placed[i] = p, true
			known++
		}
	}
	neighbours := g.Neighbours()
	// New nodes start near their placed neighbours, else on a sphere
	radius := math.Cbrt(float64(n)) * 1.5
	for pass := 0; pass < 3; pass++ {
		for i, node := range g.Nodes {
			if placed[i] {
				continue
			}
			var sum Vec
			count := 0
			for _, j := range neighbours[i] {
				if placed[j] {
					sum = sum.add(pos[j])
					count++
				}
			}
			if count > 0 {
				pos[i], placed[i] = sum.scale(1/float64(count)).add(jitter(node.Pubkey, 0.5)), true
			} else if pass == 2 {
				pos[i], placed[i] = jitter(node.Pubkey, radius), true
			}
		}
	}
	pinned := g.Index(ours)
	if pinned >= 0 {
		// Everything moves with our node, so it sits at the origin
		shift := pos[pinned]
		for i := range pos {
			pos[i] = pos[i].sub(shift)
		}
	}

	steps := o.ColdSteps
	if n > 0 && known*10 >= n*9 {
		steps = o.WarmSteps
	}
	// Large graphs: fewer, coarser steps (each costs n log n); the warm
	// start of the next snapshot goes on refining
	if n > 5000 {
		steps = int(float64(steps) * math.Sqrt(5000/float64(n)))
		if steps < 20 {
			steps = 20
		}
	}
	theta := o.Theta
	if n > 20000 {
		theta = math.Max(theta, 1.2)
	}
	// Edge weights: thicker channels pull a little harder
	weight := make([]float64, len(g.Edges))
	for i, e := range g.Edges {
		weight[i] = 0.5 + 0.1*math.Log1p(float64(e.Capacity)/1e6)
	}
	temperature := radius / 4
	if steps == o.WarmSteps {
		temperature = 0.3
	}
	disp := make([]Vec, n)
	for step := 0; step < steps; step++ {
		tree := build(pos)
		repel(tree, pos, disp, theta)
		for i, e := range g.Edges {
			d := pos[e.B].sub(pos[e.A])
			dist := d.len() + 1e-9
			f := d.scale(dist * weight[i] / dist) // spring: force grows with length
			disp[e.A] = disp[e.A].add(f)
			disp[e.B] = disp[e.B].sub(f)
		}
		for i := range pos {
			disp[i] = disp[i].sub(pos[i].scale(o.Gravity))
			if i == pinned {
				disp[i] = Vec{}
				continue
			}
			d := disp[i].len()
			if d > temperature {
				disp[i] = disp[i].scale(temperature / d)
			}
			pos[i] = pos[i].add(disp[i])
			disp[i] = Vec{}
		}
		temperature *= 0.97
		if temperature < 0.02 {
			temperature = 0.02
		}
	}
	return pos
}

// --- Barnes-Hut octree ---------------------------------------------------

type cell struct {
	center Vec // of mass
	mass   float64
	lo, hi Vec
	child  [8]int32 // -1 empty
	leaf   bool
	body   int32   // a leaf's body, or -1
	extra  []int32 // more bodies, only past the depth limit
}

type tree struct{ cells []cell }

const maxDepth = 40

func build(pos []Vec) *tree {
	lo := Vec{math.Inf(1), math.Inf(1), math.Inf(1)}
	hi := Vec{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
	for _, p := range pos {
		for k := 0; k < 3; k++ {
			lo[k] = math.Min(lo[k], p[k])
			hi[k] = math.Max(hi[k], p[k])
		}
	}
	size := 1e-6
	for k := 0; k < 3; k++ {
		size = math.Max(size, hi[k]-lo[k]+1e-6)
	}
	hi = lo.add(Vec{size, size, size})
	t := &tree{cells: make([]cell, 0, 2*len(pos)+1)}
	t.cells = append(t.cells, newCell(lo, hi))
	for i := range pos {
		t.insert(0, int32(i), pos, 0)
	}
	t.weigh(0, pos)
	return t
}

func newCell(lo, hi Vec) cell {
	c := cell{lo: lo, hi: hi, leaf: true, body: -1}
	for i := range c.child {
		c.child[i] = -1
	}
	return c
}

func (t *tree) insert(ci int, b int32, pos []Vec, depth int) {
	c := &t.cells[ci]
	if c.leaf {
		if c.body < 0 {
			c.body = b
			return
		}
		if depth >= maxDepth {
			c.extra = append(c.extra, b)
			return
		}
		// Split: the resident goes down, then the newcomer
		resident := c.body
		c.body, c.leaf = -1, false
		t.down(ci, resident, pos, depth)
	}
	t.down(ci, b, pos, depth)
}

func (t *tree) down(ci int, b int32, pos []Vec, depth int) {
	c := &t.cells[ci]
	mid := c.lo.add(c.hi).scale(0.5)
	o := 0
	lo, hi := c.lo, mid
	for k := 0; k < 3; k++ {
		if pos[b][k] >= mid[k] {
			o |= 1 << k
			lo[k], hi[k] = mid[k], c.hi[k]
		}
	}
	ch := c.child[o]
	if ch < 0 {
		t.cells = append(t.cells, newCell(lo, hi))
		ch = int32(len(t.cells) - 1)
		t.cells[ci].child[o] = ch
	}
	t.insert(int(ch), b, pos, depth+1)
}

func (t *tree) weigh(ci int, pos []Vec) (Vec, float64) {
	var sum Vec
	m := 0.0
	if c := &t.cells[ci]; c.leaf {
		if c.body >= 0 {
			sum, m = pos[c.body], 1
		}
		for _, b := range c.extra {
			sum = sum.add(pos[b])
			m++
		}
	} else {
		for _, ch := range t.cells[ci].child {
			if ch >= 0 {
				cc, cm := t.weigh(int(ch), pos)
				sum = sum.add(cc.scale(cm))
				m += cm
			}
		}
	}
	center := sum
	if m > 0 {
		center = sum.scale(1 / m)
	}
	t.cells[ci].center, t.cells[ci].mass = center, m
	return center, m
}

// pair is the repulsion on body i at p from body j (or a cell, j < 0)
// at q.
func pair(p, q Vec, mass float64, i, j int) Vec {
	d := p.sub(q)
	dist := d.len()
	if dist < 1e-6 {
		// The same place: a stable nudge, opposite for the two of a pair
		lo, hi := i, j
		if lo > hi {
			lo, hi = hi, lo
		}
		u := jitter(fmt.Sprint(lo, hi), 1)
		if i > j {
			u = u.scale(-1)
		}
		return u.scale(mass)
	}
	return d.scale(mass / (dist * dist * dist))
}

// repel adds every node's repulsion from all others, through the tree.
// Each node's force depends only on the tree, so nodes are done in
// parallel with the same result as in sequence.
func repel(t *tree, pos []Vec, disp []Vec, theta float64) {
	workers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	chunk := (len(pos) + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start, end := w*chunk, (w+1)*chunk
		if end > len(pos) {
			end = len(pos)
		}
		if start >= end {
			break
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			stack := make([]int32, 0, 256)
			for i := start; i < end; i++ {
				var f Vec
				p := pos[i]
				stack = append(stack[:0], 0)
				for len(stack) > 0 {
					c := &t.cells[stack[len(stack)-1]]
					stack = stack[:len(stack)-1]
					if c.mass == 0 {
						continue
					}
					if c.leaf {
						if int(c.body) != i {
							f = f.add(pair(p, pos[c.body], 1, i, int(c.body)))
						}
						for _, b := range c.extra {
							if int(b) != i {
								f = f.add(pair(p, pos[b], 1, i, int(b)))
							}
						}
						continue
					}
					dist := p.sub(c.center).len()
					if (c.hi[0]-c.lo[0])/(dist+1e-9) < theta {
						f = f.add(pair(p, c.center, c.mass, i, -1))
						continue
					}
					for _, ch := range c.child {
						if ch >= 0 {
							stack = append(stack, ch)
						}
					}
				}
				disp[i] = disp[i].add(f)
			}
		}(start, end)
	}
	wg.Wait()
}
