// Command graph writes the Lightning Fork network's sky: the channel
// graph from lnd (read only), laid out in 3D, as static files nginx serves
// under /graph/ (meta.json, then v<N>/...).
//
//	LND_REST_URL   https://lnd:8080
//	LND_TLS_CERT   /lnd/tls.cert
//	GRAPH_MACAROON /lnd/graph.macaroon (GetInfo, DescribeGraph)
//	OUT_DIR        /out (served)
//	STATE_FILE     /state/state.json (the layout, kept between rounds)
//	INTERVAL       10m
//	ONCE           1: one round, then exit
//	GRAPH_FILE     read the graph from this file (lncli describegraph's
//	               output) instead of lnd, with OUR_PUBKEY and OUR_URIS
//	               (comma-separated): for building and testing the sky
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paulscode/lightning-fork-swap/graph/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/graph/internal/run"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type fileSource struct {
	path string
	info lnd.Info
}

func (f fileSource) GetInfo(context.Context) (lnd.Info, error) { return f.info, nil }
func (f fileSource) DescribeGraph(context.Context) ([]byte, error) {
	return os.ReadFile(f.path)
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var client run.Source
	if file := os.Getenv("GRAPH_FILE"); file != "" {
		client = fileSource{path: file, info: lnd.Info{IdentityPubkey: os.Getenv("OUR_PUBKEY"),
			URIs: strings.FieldsFunc(os.Getenv("OUR_URIS"), func(r rune) bool { return r == ',' })}}
	} else {
		c, err := lnd.New(env("LND_REST_URL", "https://lnd:8080"),
			env("LND_TLS_CERT", "/lnd/tls.cert"), env("GRAPH_MACAROON", "/lnd/graph.macaroon"))
		if err != nil {
			logger.Fatal(err)
		}
		client = c
	}
	interval, err := time.ParseDuration(env("INTERVAL", "10m"))
	if err != nil || interval < time.Minute {
		interval = 10 * time.Minute
	}
	out, state := env("OUT_DIR", "/out"), env("STATE_FILE", "/state/state.json")
	for {
		start := time.Now()
		version, fresh, err := run.Round(ctx, client, out, state, time.Now().UTC())
		switch {
		case err != nil:
			logger.Printf("round: %v", err)
		case fresh:
			logger.Printf("version %d written in %v", version, time.Since(start).Round(time.Millisecond))
		}
		if os.Getenv("ONCE") == "1" {
			if err != nil {
				os.Exit(1)
			}
			return
		}
		wait := interval
		if err != nil {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
