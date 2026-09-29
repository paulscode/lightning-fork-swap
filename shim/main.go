// txindex-shim stands in for -txindex in front of a pruned Bitcoin node.
//
// Bitcoin Core and Knots answer getrawtransaction for a confirmed transaction
// only with -txindex or when told which block holds it, and -txindex cannot be
// combined with pruning. This proxy forwards every JSON-RPC request to the
// node unchanged. When the node answers a getrawtransaction that named no
// block with "No such mempool or blockchain transaction", the proxy looks the
// transaction up in its own index of recent blocks and asks again with the
// block hash. The index covers a window of recent blocks (-window), which is
// all a swap service needs: the transactions it looks up are hours to days
// old.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	window, _ := strconv.ParseInt(env("SHIM_WINDOW", "4032"), 10, 64)

	listen := flag.String("listen", env("SHIM_LISTEN", ":8332"), "address to serve JSON-RPC on")
	upstream := flag.String("upstream", env("SHIM_UPSTREAM", "http://127.0.0.1:8332/"), "the node's JSON-RPC URL")
	user := flag.String("rpcuser", env("SHIM_RPCUSER", ""), "RPC user the shim indexes with")
	password := flag.String("rpcpassword", env("SHIM_RPCPASSWORD", ""), "RPC password the shim indexes with")
	cookie := flag.String("rpccookiefile", env("SHIM_RPCCOOKIEFILE", ""), "cookie file, instead of user and password")
	dbPath := flag.String("db", env("SHIM_DB", "/data/txindex.db"), "index database")
	flag.Int64Var(&window, "window", window, "number of recent blocks to index")
	poll := flag.Duration("poll", time.Second, "how often to look for new blocks")
	forwardSpec := flag.String("forward", env("SHIM_FORWARD", ""), "TCP ports to relay, e.g. 28332=knots:28332,28333=knots:28333")
	flag.Parse()

	if *cookie != "" {
		raw, err := os.ReadFile(*cookie)
		if err != nil {
			log.Fatalf("reading cookie: %v", err)
		}
		u, p, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
		if !ok {
			log.Fatal("malformed cookie file")
		}
		*user, *password = u, p
	}

	ix, err := openIndex(*dbPath)
	if err != nil {
		log.Fatalf("opening index: %v", err)
	}
	defer ix.close()

	rpc := newRPCClient(*upstream, *user, *password)
	idx := &indexer{rpc: rpc, ix: ix, window: window}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	forwards, err := parseForwards(*forwardSpec)
	if err != nil {
		log.Fatal(err)
	}
	for listenAddr, target := range forwards {
		go func() {
			if err := forward(ctx, listenAddr, target); err != nil {
				log.Fatalf("forward %s: %v", listenAddr, err)
			}
		}()
	}

	go func() {
		for {
			if err := idx.sync(ctx); err != nil && ctx.Err() == nil {
				log.Printf("sync: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(*poll):
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/", &proxy{upstream: *upstream, ix: ix, http: &http.Client{Timeout: 5 * time.Minute}, catchUp: idx.catchUp})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		tip, _ := ix.tip()
		n, _ := ix.count()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"tip": tip, "transactions": int64(n), "window": window})
	})

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("serving on %s, forwarding to %s, window %d blocks", *listen, *upstream, window)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
