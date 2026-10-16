package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
)

// chain is a fake explorer.
type chain struct {
	txs       map[string]*esplora.Tx
	statuses  map[string]*esplora.Status
	outspends map[string]*esplora.Outspend
	fail      error
	calls     int
	// Parents known only by a status are transactions mined from new
	// coins (the BLAKE2b side's shorthand)
	parents bool
}

func (c *chain) Tx(_ context.Context, txid string) (*esplora.Tx, error) {
	c.calls++
	if c.fail != nil {
		return nil, c.fail
	}
	if tx, ok := c.txs[txid]; ok {
		return tx, nil
	}
	if s, ok := c.statuses[txid]; ok && c.parents {
		return &esplora.Tx{Txid: txid, Status: *s,
			Vin: []esplora.Vin{{IsCoinbase: true}}}, nil
	}
	return nil, esplora.ErrNotFound
}

func (c *chain) TxStatus(_ context.Context, txid string) (*esplora.Status, error) {
	c.calls++
	if c.fail != nil {
		return nil, c.fail
	}
	if s, ok := c.statuses[txid]; ok {
		return s, nil
	}
	return nil, esplora.ErrNotFound
}

func (c *chain) Outspend(_ context.Context, txid string, vout uint32) (*esplora.Outspend, error) {
	c.calls++
	if c.fail != nil {
		return nil, c.fail
	}
	if o, ok := c.outspends[txid]; ok {
		return o, nil
	}
	return &esplora.Outspend{}, nil
}

var (
	donation = strings.Repeat("d", 64)
	prevA    = strings.Repeat("a", 64)
	prevB    = strings.Repeat("b", 64)
	other    = strings.Repeat("e", 64)
	sig64    = strings.Repeat("11", 64)
	sig65u   = strings.Repeat("11", 64) + "21"
)

func vin(prev string, witness ...string) esplora.Vin {
	return esplora.Vin{Txid: prev, Vout: 1, Witness: witness,
		Prevout: &esplora.Prevout{Type: "v1_p2tr", Address: "bc1p" + prev[:8], Value: 1000}}
}

func blake2b(prevAHeight, prevBHeight int64, vins ...esplora.Vin) *chain {
	return &chain{parents: true,
		txs: map[string]*esplora.Tx{donation: {Txid: donation, Vin: vins}},
		statuses: map[string]*esplora.Status{
			prevA: {Confirmed: true, BlockHeight: prevAHeight},
			prevB: {Confirmed: true, BlockHeight: prevBHeight},
		},
	}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	preFork := int64(ForkHeight - 1)
	cases := []struct {
		name    string
		blake   *chain
		sha     *chain
		verdict Verdict
	}{
		{"a post-fork coin protects the whole transaction",
			blake2b(preFork, ForkHeight, vin(prevA, sig64), vin(prevB, sig64)),
			&chain{}, Protected},
		{"a post-fork coin whose parent was copied to the SHA256 chain protects nothing",
			blake2b(preFork, ForkHeight, vin(prevA, sig64), vin(prevB, sig64)),
			&chain{txs: map[string]*esplora.Tx{prevB: {Txid: prevB,
				Status: esplora.Status{Confirmed: true, BlockHeight: 970000}}}}, AtRisk},
		{"a post-fork coin whose parent waits in a SHA256 mempool protects nothing",
			blake2b(ForkHeight, ForkHeight, vin(prevA, sig64)),
			&chain{txs: map[string]*esplora.Tx{prevA: {Txid: prevA}}}, AtRisk},
		{"a status the SHA256 explorer gives for any txid is not a parent",
			blake2b(ForkHeight, ForkHeight, vin(prevA, sig64)),
			&chain{statuses: map[string]*esplora.Status{
				prevA: {Confirmed: false}}}, Protected},
		{"a post-fork coin, the SHA256 explorers down: unknown",
			blake2b(ForkHeight, ForkHeight, vin(prevA, sig64)),
			&chain{fail: errors.New("down")}, Unknown},
		{"one unified signature protects it",
			blake2b(preFork, preFork, vin(prevA, sig64), vin(prevB, sig65u)),
			&chain{}, Protected},
		{"unprotected pre-fork coins still unspent there: at risk",
			blake2b(preFork, preFork, vin(prevA, sig64), vin(prevB, sig64)),
			&chain{}, AtRisk},
		{"the donor moved them there first",
			blake2b(preFork, preFork, vin(prevA, sig64), vin(prevB, sig64)),
			&chain{outspends: map[string]*esplora.Outspend{
				prevB: {Spent: true, Txid: other}}}, TwinMoved},
		{"copied and confirmed there",
			blake2b(preFork, preFork, vin(prevA, sig64)),
			&chain{statuses: map[string]*esplora.Status{
				donation: {Confirmed: true, BlockHeight: 970000}}}, Replayed},
		{"copied, still in a mempool there: at risk",
			blake2b(preFork, preFork, vin(prevA, sig64)),
			&chain{statuses: map[string]*esplora.Status{
				donation: {Confirmed: false}},
				outspends: map[string]*esplora.Outspend{
					prevA: {Spent: true, Txid: donation}}}, AtRisk},
		{"the SHA256 explorers are down: unknown",
			blake2b(preFork, preFork, vin(prevA, sig64)),
			&chain{fail: errors.New("down")}, Unknown},
		{"an unreadable input: unknown",
			blake2b(preFork, preFork, vin(prevA, "zz")),
			&chain{}, Unknown},
		{"an unconfirmed parent from new coins can't be there",
			&chain{parents: true, txs: map[string]*esplora.Tx{donation: {Txid: donation,
				Vin: []esplora.Vin{vin(prevA, sig64)}}},
				statuses: map[string]*esplora.Status{prevA: {Confirmed: false}}},
			&chain{}, Protected},
		{"a coinbase input is new coins",
			&chain{txs: map[string]*esplora.Tx{donation: {Txid: donation,
				Vin: []esplora.Vin{{IsCoinbase: true}}}}},
			&chain{}, Protected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Check(ctx, donation, c.blake, c.sha)
			if err != nil {
				t.Fatal(err)
			}
			if got.Verdict != c.verdict {
				t.Fatalf("got %s (%s), want %s", got.Verdict, got.Reason, c.verdict)
			}
			if got.Verdict == AtRisk && len(got.AtRiskAddresses) == 0 {
				t.Fatal("at risk without the donor's addresses")
			}
		})
	}
}

func TestCheckLooksUpNoMoreThanItNeeds(t *testing.T) {
	ctx := context.Background()
	preFork := int64(ForkHeight - 1)
	// The first input settles it: the rest are not looked up
	blake := blake2b(ForkHeight, preFork, vin(prevA, sig64), vin(prevB, sig64), vin(prevB, sig64))
	sha := &chain{}
	got, err := Check(ctx, donation, blake, sha)
	if err != nil || got.Verdict != Protected {
		t.Fatalf("%v %v", got.Verdict, err)
	}
	// The donation and its first parent here; the parent's absence there
	if blake.calls != 2 || sha.calls != 1 {
		t.Fatalf("%d BLAKE2b and %d SHA256 requests", blake.calls, sha.calls)
	}

	// Too many inputs: left for a look by hand, nothing looked up
	var many []esplora.Vin
	for i := 0; i <= MaxInputs; i++ {
		many = append(many, vin(prevA, sig64))
	}
	blake = blake2b(preFork, preFork, many...)
	sha = &chain{}
	got, err = Check(ctx, donation, blake, sha)
	if err != nil || got.Verdict != Unknown || !strings.Contains(got.Reason, "by hand") {
		t.Fatalf("%v (%s) %v", got.Verdict, got.Reason, err)
	}
	if blake.calls != 1 || sha.calls != 0 {
		t.Fatalf("%d BLAKE2b and %d SHA256 requests", blake.calls, sha.calls)
	}
}

func TestCheckReportsInputs(t *testing.T) {
	got, err := Check(context.Background(), donation,
		blake2b(ForkHeight-1, ForkHeight-1, vin(prevA, sig64), vin(prevB, sig64)),
		&chain{outspends: map[string]*esplora.Outspend{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inputs) != 2 || got.Inputs[0].Prevout != prevA+":1" ||
		!got.Inputs[0].PreFork || got.Inputs[0].Sha256 != "unspent" ||
		got.Inputs[0].HashTypes[0] != "0x00" {
		t.Fatalf("inputs: %+v", got.Inputs)
	}
	if len(got.AtRiskAddresses) != 2 {
		t.Fatalf("addresses: %v", got.AtRiskAddresses)
	}
}

func TestCheckNeedsTheDonation(t *testing.T) {
	if _, err := Check(context.Background(), donation, &chain{}, &chain{}); err == nil {
		t.Fatal("a missing donation was judged")
	}
	b := blake2b(ForkHeight-1, 0, vin(prevA, sig64))
	delete(b.statuses, prevA)
	got, err := Check(context.Background(), donation, b, &chain{})
	if err != nil || got.Verdict != Unknown {
		t.Fatalf("a parent the explorer does not know: %v %v", got.Verdict, err)
	}
}

// ancestry builds, on the BLAKE2b chain, a donation spending a coin made
// after the fork by a chain of n transactions that starts from a coin made
// before it (prevB); sig signs every one of them
func ancestry(n int, sig string) (*chain, []string) {
	b := &chain{txs: map[string]*esplora.Tx{}, statuses: map[string]*esplora.Status{}}
	b.txs[prevB] = &esplora.Tx{Txid: prevB,
		Status: esplora.Status{Confirmed: true, BlockHeight: ForkHeight - 5}}
	ids := []string{}
	from := prevB
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%064x", i+1)
		b.txs[id] = &esplora.Tx{Txid: id, Vin: []esplora.Vin{vin(from, sig)},
			Status: esplora.Status{Confirmed: true, BlockHeight: ForkHeight + int64(i)}}
		ids = append(ids, id)
		from = id
	}
	b.txs[donation] = &esplora.Tx{Txid: donation, Vin: []esplora.Vin{vin(from, sig64)}}
	return b, ids
}

func TestCheckFollowsTheAncestry(t *testing.T) {
	ctx := context.Background()

	// Three transactions after the fork, none on the SHA256 chain yet, all
	// copyable: the donation can follow them there
	b, _ := ancestry(3, sig64)
	got, err := Check(ctx, donation, b, &chain{})
	if err != nil || got.Verdict != AtRisk || got.Inputs[0].Sha256 != "copyable" {
		t.Fatalf("copyable ancestry: %v (%s) %+v %v", got.Verdict, got.Reason, got.Inputs, err)
	}

	// One of them signed unified: it can never get there, nor can we
	b, ids := ancestry(3, sig64)
	b.txs[ids[1]].Vin[0].Witness = []string{sig65u}
	got, _ = Check(ctx, donation, b, &chain{})
	if got.Verdict != Protected || !strings.Contains(got.Reason, "UNIFIED") {
		t.Fatalf("a unified ancestor: %v (%s)", got.Verdict, got.Reason)
	}

	// The coin from before the fork was spent on the SHA256 chain by
	// something else: the ancestry can't be copied
	b, _ = ancestry(3, sig64)
	got, _ = Check(ctx, donation, b, &chain{outspends: map[string]*esplora.Outspend{
		prevB: {Spent: true, Txid: other}}})
	if got.Verdict != Protected || !strings.Contains(got.Reason, "another transaction") {
		t.Fatalf("a moved early coin: %v (%s)", got.Verdict, got.Reason)
	}

	// The middle one already copied there: from it on, the coins exist
	b, ids = ancestry(3, sig64)
	got, _ = Check(ctx, donation, b, &chain{txs: map[string]*esplora.Tx{
		ids[1]: {Txid: ids[1], Status: esplora.Status{Confirmed: true}}}})
	if got.Verdict != AtRisk {
		t.Fatalf("a copied ancestor: %v (%s)", got.Verdict, got.Reason)
	}

	// Longer than is followed: unknown, never protected
	b, _ = ancestry(MaxAncestors+5, sig64)
	got, _ = Check(ctx, donation, b, &chain{})
	if got.Verdict != Unknown || !strings.Contains(got.Reason, "by hand") {
		t.Fatalf("a long ancestry: %v (%s)", got.Verdict, got.Reason)
	}
}

func TestNextCheck(t *testing.T) {
	seen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(days int) time.Time { return seen.Add(time.Duration(days) * 24 * time.Hour) }
	cases := []struct {
		v    Verdict
		days int
		want time.Duration // 0 = never
	}{
		{Pending, 0, time.Minute},
		{AtRisk, 1, time.Hour},
		{AtRisk, 31, 24 * time.Hour},
		{AtRisk, 366, 0},
		{Unknown, 6, time.Hour},
		{Unknown, 8, 24 * time.Hour},
		{Unknown, 31, 0},
		{Protected, 0, 0},
		{TwinMoved, 0, 0},
		{Replayed, 0, 0},
	}
	for _, c := range cases {
		got := NextCheck(c.v, seen, at(c.days))
		if c.want == 0 && !got.IsZero() || c.want != 0 && got.Sub(at(c.days)) != c.want {
			t.Errorf("%s after %d days: %v", c.v, c.days, got)
		}
		if c.v.Final() != (c.v == Protected || c.v == TwinMoved || c.v == Replayed) {
			t.Errorf("%s final", c.v)
		}
	}
}
