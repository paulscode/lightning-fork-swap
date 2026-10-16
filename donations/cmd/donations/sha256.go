package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paulscode/lightning-fork-swap/donations/internal/channels"
	"github.com/paulscode/lightning-fork-swap/donations/internal/knots"
	"github.com/paulscode/lightning-fork-swap/donations/internal/sha256don"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

func sha256API(pool *pgxpool.Pool) *sha256don.API {
	return &sha256don.API{
		Store: &sha256don.Postgres{Pool: pool}, Params: &chaincfg.MainNetParams,
		PoW:         channels.NewPoW(int(number("SHA256_POW_BITS", 18))),
		MaxPerDay:   int(number("SHA256_MAX_PER_DAY", 300)),
		Now:         time.Now,
		AddressWait: 4 * time.Second,
		Sha256Confs: sha256don.DefaultSha256Confs, Blake2bConfs: sha256don.DefaultBlake2bConfs,
	}
}

// secret reads KEY, or the file named by KEY_FILE.
func secret(key string) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	path := os.Getenv(key + "_FILE")
	if path == "" {
		return "", fmt.Errorf("%s or %s_FILE is needed", key, key)
	}
	raw, err := os.ReadFile(path)
	return strings.TrimSpace(string(raw)), err
}

func sha256Keys() (*sha256don.Keys, error) {
	return sha256don.LoadKeys(env("SHA256_SEED_FILE", "/secrets/sha256-donations.seed"),
		&chaincfg.MainNetParams)
}

func runSHA256(ctx context.Context, logger *log.Logger) error {
	db, err := store.Open(ctx, env("DONATIONS_DB_URL", ""), true)
	if err != nil {
		return err
	}
	defer db.Pool.Close()
	if _, err := db.Pool.Exec(ctx, sha256don.Schema); err != nil {
		return err
	}
	keys, err := sha256Keys()
	if err != nil {
		return fmt.Errorf("the seed: %w", err)
	}
	password, err := secret("KNOTS_RPC_PASSWORD")
	if err != nil {
		return err
	}
	node := knots.New(env("KNOTS_RPC_URL", "http://knots:8332"),
		env("KNOTS_RPC_USER", "donations"), password)
	if _, err := node.Height(ctx); err != nil {
		return fmt.Errorf("our Knots node: %w", err)
	}
	lndAddress := env("SHA256_LND_ADDRESS", "")
	if lndAddress == "" {
		if a := list("DONATION_ADDRESSES", ""); len(a) > 0 {
			lndAddress = a[0]
		}
	}
	if lndAddress == "" {
		logger.Print("no SHA256_LND_ADDRESS nor DONATION_ADDRESSES: coins without a return address wait")
	}
	blake2b, sha256 := explorers()
	w := &sha256don.Worker{Store: &sha256don.Postgres{Pool: db.Pool}, Keys: keys,
		Params: &chaincfg.MainNetParams, Sha256: sha256, Blake2b: blake2b, Node: node,
		LndAddress: lndAddress, Now: time.Now, Logger: logger}
	wake := make(chan struct{}, 1)
	go listen(ctx, db.Pool, "sha256_requests", wake, logger)
	logger.Printf("SHA256 donations: running (coins without a return address to %s)", lndAddress)
	w.Run(ctx, 30*time.Second, wake)
	return nil
}

func runSHA256CLI(ctx context.Context, args []string) error {
	usage := errors.New("usage: donations sha256 list [--all] | show ID | export [--private]")
	if len(args) == 0 {
		return usage
	}
	if args[0] == "export" {
		keys, err := sha256Keys()
		if err != nil {
			return err
		}
		desc, err := keys.Descriptor(len(args) > 1 && args[1] == "--private")
		if err != nil {
			return err
		}
		fmt.Println(desc)
		return nil
	}
	db, err := store.Open(ctx, os.Getenv("DONATIONS_DB_URL"), false)
	if err != nil {
		return err
	}
	defer db.Pool.Close()
	s := &sha256don.Postgres{Pool: db.Pool}
	switch args[0] {
	case "list":
		reqs, err := s.Assigned(ctx)
		if err != nil {
			return err
		}
		for _, r := range reqs {
			ds, err := s.Deposits(ctx, r.ID)
			if err != nil {
				return err
			}
			if len(ds) == 0 && (len(args) < 2 || args[1] != "--all") {
				continue
			}
			var sha, blake int64
			for _, d := range ds {
				if d.State == sha256don.Gone {
					continue
				}
				if d.Chain == sha256don.SHA256 {
					sha += d.ValueSat
				} else {
					blake += d.ValueSat
				}
			}
			ret := r.ReturnAddress
			if ret == "" {
				ret = "(lnd)"
			}
			fmt.Printf("%s  %s  SHA256 %12d sat  BLAKE2b %12d sat  to %s  replay=%v\n",
				r.ID, r.CreatedAt.UTC().Format("2006-01-02"), sha, blake, ret, r.Replay)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return usage
		}
		r, err := s.GetRequest(ctx, args[1])
		if err != nil {
			return err
		}
		ds, err := s.Deposits(ctx, r.ID)
		if err != nil {
			return err
		}
		fs, err := s.Forwards(ctx, r.ID)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"request": r, "keyIndex": r.KeyIndex,
			"deposits": ds, "forwards": fs})
	}
	return usage
}
