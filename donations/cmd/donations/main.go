// Command donations runs the donation watcher of Lightning Fork Swap.
//
//	donations worker     find donations in lnd's wallet, check each for
//	                     replay protection (DONATIONS_DB_URL, LND_REST_URL,
//	                     LND_TLS_CERT, LND_MACAROON, DONATION_ADDRESSES)
//	donations api        serve /donate/v1/ from the database (read only;
//	                     DONATIONS_DB_URL, LISTEN)
//	donations replay list [--at-risk | --replayed | --unknown | --all]
//	donations replay show TXID
//	donations replay note TXID TEXT
//	donations replay verify TXID ADDRESS MESSAGE SIGNATURE
//	                     check a donor's proof that ADDRESS (one of the
//	                     transaction's inputs) is theirs
//	donations check TXID check a transaction now, print the result
//
// BLAKE2B_EXPLORERS and SHA256_EXPLORERS (comma-separated Esplora API URLs)
// default to mempool.guide, and mempool.space then blockstream.info.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/api"
	"github.com/paulscode/lightning-fork-swap/donations/internal/bip322"
	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
	"github.com/paulscode/lightning-fork-swap/donations/internal/worker"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func list(key, fallback string) []string {
	var out []string
	for _, item := range strings.Split(env(key, fallback), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, strings.TrimRight(item, "/"))
		}
	}
	return out
}

func explorers() (*esplora.Client, *esplora.Client) {
	return esplora.New(list("BLAKE2B_EXPLORERS", "https://mempool.guide/api")...),
		esplora.New(list("SHA256_EXPLORERS",
			"https://mempool.space/api,https://blockstream.info/api")...)
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], logger); err != nil {
		fmt.Fprintln(os.Stderr, "donations:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logger *log.Logger) error {
	if len(args) == 0 {
		return errors.New("usage: donations worker|api|replay|check")
	}
	switch args[0] {
	case "worker":
		return runWorker(ctx, logger)
	case "api":
		return runAPI(ctx, logger)
	case "check":
		if len(args) != 2 {
			return errors.New("usage: donations check TXID")
		}
		blake2b, sha256 := explorers()
		result, err := replay.Check(ctx, args[1], blake2b, sha256)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "replay":
		return runReplay(ctx, args[1:])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func runWorker(ctx context.Context, logger *log.Logger) error {
	addresses := map[string]bool{}
	for _, a := range list("DONATION_ADDRESSES", "") {
		addresses[a] = true
	}
	if len(addresses) == 0 {
		logger.Print("no DONATION_ADDRESSES: nothing to watch")
	}
	db, err := store.Open(ctx, os.Getenv("DONATIONS_DB_URL"), true)
	if err != nil {
		return err
	}
	defer db.Pool.Close()
	wallet, err := lnd.New(env("LND_REST_URL", "https://lnd:8080"),
		env("LND_TLS_CERT", "/lnd/tls.cert"),
		env("LND_MACAROON", "/lnd/donations.macaroon"))
	if err != nil {
		return err
	}
	blake2b, sha256 := explorers()
	seconds, _ := strconv.Atoi(env("POLL_SECONDS", "30"))
	w := &worker.Worker{
		Store: db, Wallet: wallet, Blake2b: blake2b, Sha256: sha256,
		Addresses: addresses, Now: time.Now, Logger: logger,
	}
	logger.Printf("watching %d donation address(es) every %ds", len(addresses), seconds)
	w.Run(ctx, time.Duration(seconds)*time.Second)
	return nil
}

func runAPI(ctx context.Context, logger *log.Logger) error {
	db, err := store.Open(ctx, os.Getenv("DONATIONS_DB_URL"), false)
	if err != nil {
		return err
	}
	defer db.Pool.Close()
	server := &http.Server{
		Addr:              env("LISTEN", ":9010"),
		Handler:           api.Handler(db),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger.Printf("listening on %s", server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runReplay(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: donations replay list|show|note|verify")
	}
	db, err := store.Open(ctx, os.Getenv("DONATIONS_DB_URL"), false)
	if err != nil {
		return err
	}
	defer db.Pool.Close()
	switch args[0] {
	case "list":
		verdicts := []replay.Verdict{replay.AtRisk, replay.Replayed, replay.Unknown}
		if len(args) > 1 {
			switch args[1] {
			case "--at-risk":
				verdicts = []replay.Verdict{replay.AtRisk}
			case "--replayed":
				verdicts = []replay.Verdict{replay.Replayed}
			case "--unknown":
				verdicts = []replay.Verdict{replay.Unknown}
			case "--all":
				verdicts = nil
			default:
				return fmt.Errorf("unknown option %q", args[1])
			}
		}
		records, err := db.List(ctx, verdicts)
		if err != nil {
			return err
		}
		for _, r := range records {
			fmt.Printf("%s  %-10s %12d sat  %s  %s\n", r.Txid, r.Verdict,
				r.AmountSat, r.FirstSeen.UTC().Format("2006-01-02"),
				strings.Join(r.AtRiskAddresses, ","))
		}
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New("usage: donations replay show TXID")
		}
		r, err := db.Get(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(r)
	case "note":
		if len(args) != 3 {
			return errors.New("usage: donations replay note TXID TEXT")
		}
		return db.SetNote(ctx, args[1], args[2])
	case "verify":
		if len(args) != 5 {
			return errors.New("usage: donations replay verify TXID ADDRESS MESSAGE SIGNATURE")
		}
		r, err := db.Get(ctx, args[1])
		if err != nil {
			return err
		}
		if err := VerifyOwnership(r, args[2], args[3], args[4]); err != nil {
			return err
		}
		fmt.Printf("verified: %s signed %q\n", args[2], args[3])
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// VerifyOwnership checks that address spent coins in the donation and
// signed the message.
func VerifyOwnership(r store.Record, address, message, signature string) error {
	found := false
	for _, in := range r.Inputs {
		if in.Address == address {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%s is not among the inputs of %s", address, r.Txid)
	}
	return bip322.Verify(address, message, signature, nil)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
