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

// A sync older than this means the index is falling behind the node
const staleAfter = 5 * time.Minute

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	window, err := strconv.ParseInt(env("SHIM_WINDOW", "4032"), 10, 64)
	if err != nil {
		log.Fatalf("SHIM_WINDOW: %v", err)
	}

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
	if window < 1 {
		// 0 would index nothing and empty the index
		log.Fatalf("the window must be at least one block, not %d", window)
	}

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
		start := time.Now()
		var warned time.Time
		for {
			if err := idx.sync(ctx); err != nil && ctx.Err() == nil {
				log.Printf("sync: %v", err)
			}
			// Said again every ten minutes while it lasts; the operator's
			// monitor looks for it
			last := idx.lastSynced.get()
			if last.IsZero() {
				last = start
			}
			if time.Since(last) > staleAfter && time.Since(warned) > 10*time.Minute {
				log.Printf("index has not synced for %s: lookups may miss new blocks", time.Since(last).Round(time.Second))
				warned = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(*poll):
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/", &proxy{
		upstream: *upstream, ix: ix, http: &http.Client{Timeout: 5 * time.Minute},
		catchUp: idx.catchUp, inActiveChain: idx.inActiveChain,
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		tip, _ := ix.tip()
		n, _ := ix.count()
		last := idx.lastSynced.get()
		w.Header().Set("Content-Type", "application/json")
		if last.IsZero() || time.Since(last) > staleAfter {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tip": tip, "transactions": n, "window": window, "lastSynced": last.UTC().Format(time.RFC3339),
		})
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
