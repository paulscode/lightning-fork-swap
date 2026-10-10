package main

import (
	"context"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paulscode/lightning-fork-swap/donations/internal/channels"
	"github.com/paulscode/lightning-fork-swap/donations/internal/guard"
	"github.com/paulscode/lightning-fork-swap/donations/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

func number(key string, fallback int64) int64 {
	v, err := strconv.ParseInt(env(key, ""), 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

// rules: the decided values, sizes overridable from the environment
func rules() channels.Rules {
	r := channels.DefaultRules()
	r.MinChannelSat = number("CHANNEL_MIN_SAT", r.MinChannelSat)
	r.MaxChannelSat = number("CHANNEL_MAX_SAT", r.MaxChannelSat)
	r.MaxWumboSat = number("CHANNEL_MAX_WUMBO_SAT", r.MaxWumboSat)
	r.FeeCeiling = uint64(number("CHANNEL_FEE_CEILING", int64(r.FeeCeiling)))
	return r
}

func channelAPI(pool *pgxpool.Pool) *channels.API {
	return &channels.API{
		Store: &channels.Postgres{Pool: pool}, Rules: rules(),
		PoW:      channels.NewPoW(int(number("CHANNEL_POW_BITS", 18))),
		Resolver: net.DefaultResolver, Now: time.Now, AddressWait: 4 * time.Second,
	}
}

func runChannels(ctx context.Context, logger *log.Logger) error {
	// The P1a schema first: the database's owner creates both
	db, err := store.Open(ctx, env("DONATIONS_DB_URL", ""), true)
	if err != nil {
		return err
	}
	defer db.Pool.Close()
	if _, err := db.Pool.Exec(ctx, channels.Schema); err != nil {
		return err
	}
	client, err := lnd.New(env("LND_REST_URL", "https://lnd:8080"),
		env("LND_TLS_CERT", "/lnd/tls.cert"),
		env("CHANNELS_MACAROON", "/lnd/channels.macaroon"))
	if err != nil {
		return err
	}
	w := &channels.Worker{Store: &channels.Postgres{Pool: db.Pool}, Lnd: client,
		Rules: rules(), Now: time.Now, Logger: logger}
	wake := make(chan struct{}, 1)
	go listen(ctx, db.Pool, wake, logger)
	logger.Print("channel donations: running")
	w.Run(ctx, 15*time.Second, wake)
	return nil
}

// listen wakes the worker when the API adds an order or an edit.
func listen(ctx context.Context, pool *pgxpool.Pool, wake chan<- struct{}, logger *log.Logger) {
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN channel_orders")
			for err == nil {
				_, err = conn.Conn().WaitForNotification(ctx)
				if err == nil {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			}
			conn.Release()
		}
		if ctx.Err() == nil {
			logger.Printf("listen: %v", err)
			time.Sleep(5 * time.Second)
		}
	}
}

type leases struct{ c *lnd.Client }

func (l leases) LeasedByUs(ctx context.Context, txid string, index uint32) (bool, error) {
	return l.c.LeasedBy(ctx, guard.LeaseID, txid, index)
}

func runGuard(ctx context.Context, logger *log.Logger) error {
	cert := env("LND_TLS_CERT", "/lnd/tls.cert")
	mac := env("GUARD_MACAROON", "/lnd/guard.macaroon")
	conn, err := guard.Dial(env("LND_GRPC", "lnd:10009"), cert, mac)
	if err != nil {
		return err
	}
	defer conn.Close()
	rest, err := lnd.New(env("LND_REST_URL", "https://lnd:8080"), cert, mac)
	if err != nil {
		return err
	}
	p := guard.DefaultPolicy()
	p.MaxChannelSat = rules().MaxChannelSat
	if w := rules().MaxWumboSat; w > p.MaxChannelSat {
		p.MaxChannelSat = w
	}
	p.MaxFeeRate = rules().FeeCeiling
	p.Leases = leases{rest}
	p.Released = guard.NewReleased()
	logger.Printf("guard: channels up to %d sat, fees up to %d sat/vB", p.MaxChannelSat, p.MaxFeeRate)
	guard.Run(ctx, conn, p, logger)
	return nil
}
