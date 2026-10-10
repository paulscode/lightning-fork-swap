package model

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	a, b := "02"+strings.Repeat("a", 64), "03"+strings.Repeat("b", 64)
	raw := `{"nodes": [
	  {"pub_key": "` + b + `", "alias": "  bob\u0000\u001b[31m  ", "color": "#ABCDEF"},
	  {"pub_key": "` + a + `", "alias": "` + strings.Repeat("é", 40) + `", "color": "red",
	   "addresses": [{"addr": "1.2.3.4:9735"}, {"addr": ""}]},
	  {"pub_key": "04` + strings.Repeat("c", 64) + `"},
	  {"pub_key": "` + strings.ToUpper(a) + `"}
	], "edges": [
	  {"channel_id": "9", "node1_pub": "` + a + `", "node2_pub": "` + b + `", "capacity": "5000000"},
	  {"channel_id": "9", "node1_pub": "` + a + `", "node2_pub": "` + b + `", "capacity": "5000000"},
	  {"channel_id": "8", "node1_pub": "` + a + `", "node2_pub": "` + a + `", "capacity": "1"},
	  {"channel_id": "7", "node1_pub": "` + a + `", "node2_pub": "02nobody", "capacity": "1"},
	  {"channel_id": "6", "node1_pub": "` + b + `", "node2_pub": "` + a + `", "capacity": 3000000}
	]}`
	g, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 || g.Nodes[0].Pubkey != a || g.Nodes[1].Pubkey != b {
		t.Fatalf("%+v", g.Nodes)
	}
	if g.Nodes[1].Alias != "bob[31m" || g.Nodes[1].Color != "#abcdef" || g.Nodes[0].Color != "#3399ff" {
		t.Fatalf("%q %q %q", g.Nodes[1].Alias, g.Nodes[1].Color, g.Nodes[0].Color)
	}
	if len(g.Nodes[0].Alias) > 32 || len(g.Nodes[0].Addresses) != 1 {
		t.Fatalf("%q %v", g.Nodes[0].Alias, g.Nodes[0].Addresses)
	}
	if len(g.Edges) != 2 || g.Nodes[0].Capacity != 8_000_000 || g.Nodes[0].Degree != 2 ||
		g.TotalCapacity() != 8_000_000 {
		t.Fatalf("%+v %+v", g.Edges, g.Nodes[0])
	}
	if g.Edges[0].Seed != Seed("6") || Seed("6") == Seed("9") {
		t.Fatal("seeds")
	}
	imp := g.Importance(b)
	if imp[1] <= imp[0] {
		t.Fatal("ours not first")
	}
	if _, err := Parse([]byte("{")); err == nil {
		t.Fatal("garbage parsed")
	}
}
