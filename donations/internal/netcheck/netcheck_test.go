package netcheck

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for _, a := range []string{"91.190.100.60", "8.8.8.8", "2a01:4f8::1", "::ffff:91.190.100.60"} {
		if !PublicIP(netip.MustParseAddr(a)) {
			t.Errorf("%s refused", a)
		}
	}
	for _, a := range []string{
		"127.0.0.1", "10.1.2.3", "172.17.0.1", "172.30.90.10", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "0.1.2.3", "224.0.0.1",
		"255.255.255.255", "240.0.0.1", "192.0.2.1", "198.18.0.5",
		"::1", "::", "fe80::1", "fc00::1", "fd12::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1",
		"2002:7f00:1::", "2001:db8::1", "2001::1",
	} {
		if PublicIP(netip.MustParseAddr(a)) {
			t.Errorf("%s accepted", a)
		}
	}
}

func TestLiteral(t *testing.T) {
	onion := "uo4swnsgfzlstnyx44eimndqkykz5aqbep7bqmwwg42foofgr7h2pqyd.onion"
	for in, want := range map[string]string{
		"91.190.100.60:9735":       "91.190.100.60:9735",
		"[2a01:4f8::1]:9735":       "[2a01:4f8::1]:9735",
		"[::ffff:91.190.100.60]:1": "91.190.100.60:1",
		onion + ":9735":            onion + ":9735",
		"UO4SWNSGFZLSTNYX44EIMNDQKYKZ5AQBEP7BQMWWG42FOOFGR7H2PQYD.ONION:9735": onion + ":9735",
	} {
		got, err := Literal(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{
		"127.0.0.1:9735", "10.0.0.1:9735", "example.com:9735", "91.190.100.60",
		"91.190.100.60:0", "91.190.100.60:65536", "abc.onion:9735", "[fe80::1%eth0]:9735", "",
	} {
		if got, err := Literal(in); err == nil {
			t.Errorf("%s accepted as %s", in, got)
		}
	}
}

type resolver map[string][]string

func (r resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, a := range r[host] {
		out = append(out, netip.MustParseAddr(a))
	}
	if len(out) == 0 {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func TestResolve(t *testing.T) {
	r := resolver{
		"node.example.com": {"2a01:4f8::1", "91.190.100.60"},
		"rebind.example":   {"91.190.100.60", "127.0.0.1"},
		"inside.example":   {"10.0.0.5"},
	}
	ctx := context.Background()
	for in, want := range map[string]string{
		"node.example.com:9736": "91.190.100.60:9736",
		"node.example.com":      "91.190.100.60:9735",
		"Node.Example.Com.":     "91.190.100.60:9735",
		"8.8.8.8":               "8.8.8.8:9735",
		"2a01:4f8::1":           "[2a01:4f8::1]:9735",
		"[2a01:4f8::1]":         "[2a01:4f8::1]:9735",
	} {
		got, err := Resolve(ctx, r, in, 9735)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{
		"rebind.example", "inside.example", "localhost", "localhost:9735",
		"missing.example", "127.0.0.1", "1.2.3", "a..b", "x.onion",
		"node.example.com:99999", "-bad.example", "evil.123",
	} {
		if got, err := Resolve(ctx, r, in, 9735); err == nil {
			t.Errorf("%s accepted as %s", in, got)
		}
	}
}
