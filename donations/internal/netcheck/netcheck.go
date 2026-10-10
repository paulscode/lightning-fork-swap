// Package netcheck decides whether a node address a donor gave may be
// dialled by our lnd: only public internet addresses and Tor onion
// services, never this host, its Docker networks or its neighbours.
package netcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Not public, beyond what netip already knows as private, loopback,
// link-local, multicast or unspecified
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), // documentation
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),  // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64: an IPv4 address inside
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"), // IETF protocol assignments (Teredo inside)
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4: an IPv4 address inside
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fec0::/10"),
}

// PublicIP reports whether ip is a public unicast address.
func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() ||
		ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip.IsUnspecified() || ip.Zone() != "" {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var onion = regexp.MustCompile(`^[a-z2-7]{56}\.onion$`)

// IsOnion reports whether host is a Tor v3 onion name.
func IsOnion(host string) bool { return onion.MatchString(host) }

// ErrNotPublic is returned for an address our node must not dial.
var ErrNotPublic = errors.New("not a public address")

// Split parses host:port (IPv6 in brackets); without a port, defaultPort.
func Split(hostport string, defaultPort int) (string, int, error) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" || len(hostport) > 300 {
		return "", 0, errors.New("empty or too long")
	}
	host, portText, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port: a name, an IPv4 address, or an IPv6 one with or
		// without brackets
		host, portText = strings.Trim(hostport, "[]"), strconv.Itoa(defaultPort)
		if strings.Contains(host, ":") {
			if _, err := netip.ParseAddr(host); err != nil {
				return "", 0, fmt.Errorf("cannot read %q", hostport)
			}
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", portText)
	}
	return strings.TrimSuffix(strings.ToLower(host), "."), port, nil
}

// Literal checks an address that must already be an IP or an onion name
// (what the worker and the guard see): host:port, normalised.
func Literal(hostport string) (string, error) {
	host, port, err := Split(hostport, 0)
	if err != nil {
		return "", err
	}
	if IsOnion(host) {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !PublicIP(ip) {
		return "", ErrNotPublic
	}
	return net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port)), nil
}

// Resolver looks names up; net.DefaultResolver in production.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Resolve turns what a donor typed into an address to dial, once: a name
// is looked up here and its address kept, so a later lookup cannot point
// it somewhere else. Every address a name has must be public.
func Resolve(ctx context.Context, r Resolver, hostport string, defaultPort int) (string, error) {
	host, port, err := Split(hostport, defaultPort)
	if err != nil {
		return "", err
	}
	if IsOnion(host) {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	if strings.HasSuffix(host, ".onion") {
		return "", errors.New("only v3 onion addresses")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !PublicIP(ip) {
			return "", ErrNotPublic
		}
		return net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port)), nil
	}
	if !validName(host) {
		return "", fmt.Errorf("cannot read %q", host)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("%s does not resolve", host)
	}
	for _, ip := range ips {
		if !PublicIP(ip) {
			return "", ErrNotPublic
		}
	}
	// IPv4 first: it is what most nodes listen on
	chosen := ips[0]
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			chosen = ip
			break
		}
	}
	return net.JoinHostPort(chosen.Unmap().String(), strconv.Itoa(port)), nil
}

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	parts := strings.Split(host, ".")
	if len(host) > 253 || len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !label.MatchString(p) {
			return false
		}
	}
	// "localhost" and single labels are refused above; so are names whose
	// last label is all digits (they would be IPs in disguise)
	last := parts[len(parts)-1]
	_, err := strconv.Atoi(last)
	return err != nil
}
