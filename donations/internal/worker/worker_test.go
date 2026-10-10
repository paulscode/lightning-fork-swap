package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/lnd"
	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

type wallet struct {
	txs   []lnd.Transaction
	asked []int64
	fail  error
}

func (w *wallet) Transactions(_ context.Context, start int64) ([]lnd.Transaction, error) {
	w.asked = append(w.asked, start)
	return w.txs, w.fail
}

type chain struct {
	tx     *esplora.Tx
	status map[string]*esplora.Status
	fail   error
}

func (c *chain) Tx(context.Context, string) (*esplora.Tx, error) {
	if c.fail != nil {
		return nil, c.fail
	}
	return c.tx, nil
}
func (c *chain) TxStatus(_ context.Context, txid string) (*esplora.Status, error) {
	if c.fail != nil {
		return nil, c.fail
	}
	if s, ok := c.status[txid]; ok {
		return s, nil
	}
	return nil, esplora.ErrNotFound
}
func (c *chain) Outspend(context.Context, string, uint32) (*esplora.Outspend, error) {
	if c.fail != nil {
		return nil, c.fail
	}
	return &esplora.Outspend{}, nil
}

var (
	donationTx = strings.Repeat("d", 64)
	prev       = strings.Repeat("a", 64)
)

func setup() (*Worker, *store.Memory, *wallet, *chain, *chain, *time.Time) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	mem := store.NewMemory()
	w := &wallet{txs: []lnd.Transaction{
		{TxHash: donationTx, NumConfirmations: 1, BlockHeight: 976500,
			OutputDetails: []lnd.OutputDetail{{Address: "bc1pdonate", Amount: "50000"}}},
		{TxHash: strings.Repeat("e", 64), NumConfirmations: 1, BlockHeight: 976510,
			OutputDetails: []lnd.OutputDetail{{Address: "bc1qother", Amount: "9"}}},
		{TxHash: "not-a-txid", OutputDetails: []lnd.OutputDetail{
			{Address: "bc1pdonate", Amount: "1"}}},
	}}
	blake := &chain{
		tx: &esplora.Tx{Txid: donationTx, Vin: []esplora.Vin{{Txid: prev, Vout: 0,
			Witness: []string{strings.Repeat("11", 64)},
			Prevout: &esplora.Prevout{Type: "v1_p2tr", Address: "bc1pdonor", Value: 60000}}}},
		status: map[string]*esplora.Status{prev: {Confirmed: true, BlockHeight: replay.ForkHeight - 10}},
	}
	sha := &chain{status: map[string]*esplora.Status{}}
	clock := now
	wk := &Worker{Store: mem, Wallet: w, Blake2b: blake, Sha256: sha,
		Addresses: map[string]bool{"bc1pdonate": true},
		Now:       func() time.Time { return clock }}
	return wk, mem, w, blake, sha, &clock
}

func TestFindsDonationsAndChecksThem(t *testing.T) {
	wk, mem, w, _, _, _ := setup()
	// Records added now are due now; the store's clock is real time
	wk.Now = time.Now
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mem.Records) != 1 {
		t.Fatalf("records: %v", mem.Records)
	}
	rec := mem.Records[donationTx]
	if rec.Address != "bc1pdonate" || rec.AmountSat != 50000 {
		t.Fatalf("%+v", rec)
	}
	if rec.Verdict != replay.AtRisk || rec.NextCheck == nil ||
		rec.AtRiskAddresses[0] != "bc1pdonor" {
		t.Fatalf("verdict %s next %v", rec.Verdict, rec.NextCheck)
	}
	// The cursor stays a few blocks behind the newest confirmed one
	state, _ := mem.State(context.Background(), cursorKey)
	if state != "976504" {
		t.Fatalf("cursor %s", state)
	}
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.asked[1] != 976504 {
		t.Fatalf("second read from %d", w.asked[1])
	}
}

func TestAtRiskStaysAtRiskWhileExplorersAreDown(t *testing.T) {
	wk, mem, _, _, sha, clock := setup()
	wk.Now = time.Now
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	*clock = time.Now().Add(2 * time.Hour)
	wk.Now = func() time.Time { return *clock }
	sha.fail = errors.New("down")
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := mem.Records[donationTx]
	if rec.Verdict != replay.AtRisk || len(rec.AtRiskAddresses) != 1 {
		t.Fatalf("verdict %s %v", rec.Verdict, rec.AtRiskAddresses)
	}
}

func TestAProtectedDonationIsNotCheckedAgain(t *testing.T) {
	wk, mem, _, blake, _, _ := setup()
	wk.Now = time.Now
	blake.status[prev].BlockHeight = replay.ForkHeight + 1
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := mem.Records[donationTx]
	if rec.Verdict != replay.Protected || rec.NextCheck != nil {
		t.Fatalf("verdict %s next %v", rec.Verdict, rec.NextCheck)
	}
}

func TestAFailedCheckIsRecordedAsUnknown(t *testing.T) {
	wk, mem, _, blake, _, _ := setup()
	wk.Now = time.Now
	blake.fail = errors.New("mempool.guide down")
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := mem.Records[donationTx]
	if rec.Verdict != replay.Unknown || !strings.Contains(rec.Reason, "mempool.guide down") ||
		rec.NextCheck == nil {
		t.Fatalf("%+v", rec)
	}
}

func TestChannelDonationAddressesAreWatchedToo(t *testing.T) {
	wk, mem, _, _, _, _ := setup()
	wk.Now = time.Now
	wk.MoreAddresses = func(context.Context) ([]string, error) {
		return []string{"bc1qother"}, nil
	}
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mem.Records) != 2 {
		t.Fatalf("records: %v", mem.Records)
	}
	// Their source failing does not stop the fixed address
	wk, mem, _, _, _, _ = setup()
	wk.MoreAddresses = func(context.Context) ([]string, error) {
		return nil, errors.New("no such table")
	}
	_ = wk.Pass(context.Background())
	if len(mem.Records) != 1 {
		t.Fatalf("records: %v", mem.Records)
	}
}

func TestLndDownStopsThePass(t *testing.T) {
	wk, mem, w, _, _, _ := setup()
	w.fail = errors.New("lnd down")
	if err := wk.Pass(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if len(mem.Records) != 0 {
		t.Fatal("recorded without lnd")
	}
	if last, _ := mem.State(context.Background(), LastPassKey); last != "" {
		t.Fatalf("a failed pass recorded as done: %s", last)
	}
}

func TestACompletedPassIsRecorded(t *testing.T) {
	wk, mem, _, _, _, clock := setup()
	if err := wk.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	last, _ := mem.State(context.Background(), LastPassKey)
	if last != clock.Format(time.RFC3339) {
		t.Fatalf("last pass %q", last)
	}
}
