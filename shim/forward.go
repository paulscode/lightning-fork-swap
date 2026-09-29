package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
)

// forwards parses "28332=knots:28332,28333=knots:28333": listen port on the
// left, target on the right.
func parseForwards(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "=")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("bad forward %q, want port=host:port", part)
		}
		out[":"+strings.TrimPrefix(from, ":")] = to
	}
	return out, nil
}

// forward relays TCP connections on listen to target, byte for byte. The
// swap backend finds the node's ZMQ endpoints through getzmqnotifications
// and connects to them at the RPC host, which is this shim, so the shim
// passes them through.
func forward(ctx context.Context, listen, target string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	log.Printf("forwarding %s to %s", listen, target)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go relay(ctx, conn, target)
	}
}

func relay(ctx context.Context, conn net.Conn, target string) {
	defer conn.Close()
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		log.Printf("forward to %s: %v", target, err)
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go copyHalf(up, conn)
	go copyHalf(conn, up)
	wg.Wait()
}
