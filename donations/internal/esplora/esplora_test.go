package esplora

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var txid = strings.Repeat("a", 64)

func server(handler http.HandlerFunc) *httptest.Server { return httptest.NewServer(handler) }

func TestRequestsArePaced(t *testing.T) {
	var times []time.Time
	s := server(func(w http.ResponseWriter, r *http.Request) {
		times = append(times, time.Now())
		_, _ = w.Write([]byte(`{"confirmed": true}`))
	})
	defer s.Close()
	c := New(s.URL)
	c.Interval = 40 * time.Millisecond
	for i := 0; i < 4; i++ {
		if _, err := c.TxStatus(context.Background(), txid); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < 35*time.Millisecond {
			t.Fatalf("request %d came %v after the one before", i, gap)
		}
	}
	// A request that is waiting for its turn gives up with its context
	c.Interval = time.Hour
	_, _ = c.TxStatus(context.Background(), txid)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.TxStatus(ctx, txid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waited past its context: %v", err)
	}
}

// Every request names the service; Go's default user agent is refused by
// some explorers
func TestRequestsNameTheService(t *testing.T) {
	var agents []string
	s := server(func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.Method+" "+r.UserAgent())
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(txid))
			return
		}
		_, _ = w.Write([]byte(`{"confirmed": true}`))
	})
	defer s.Close()
	c := New(s.URL)
	c.Interval = 0
	if _, err := c.TxStatus(context.Background(), txid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Broadcast(context.Background(), "00"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET " + UserAgent, "POST " + UserAgent}
	if fmt.Sprint(agents) != fmt.Sprint(want) {
		t.Fatalf("user agents %q, want %q", agents, want)
	}
}

func TestFallsBackToTheNextExplorer(t *testing.T) {
	down := server(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	defer down.Close()
	up := server(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tx/"+txid+"/status" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"confirmed":true,"block_height":5}`))
	})
	defer up.Close()
	s, err := New(down.URL, up.URL).TxStatus(context.Background(), txid)
	if err != nil || !s.Confirmed || s.BlockHeight != 5 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestNotFoundOnlyWhenNoneFailed(t *testing.T) {
	missing := server(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Transaction not found", http.StatusNotFound)
	})
	defer missing.Close()
	missing400 := server(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Transaction not found"))
	})
	defer missing400.Close()
	down := server(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", http.StatusInternalServerError)
	})
	defer down.Close()

	_, err := New(missing.URL, missing400.URL).TxStatus(context.Background(), txid)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("all not found: %v", err)
	}
	_, err = New(missing.URL, down.URL).TxStatus(context.Background(), txid)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("one failed: %v", err)
	}
}

func TestRefusesWhatIsNotAsked(t *testing.T) {
	other := server(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"txid":"` + strings.Repeat("b", 64) + `","vin":[]}`))
	})
	defer other.Close()
	if _, err := New(other.URL).Tx(context.Background(), txid); err == nil {
		t.Fatal("another transaction was accepted")
	}
	c := New(other.URL)
	for _, bad := range []string{"../x", strings.Repeat("A", 64), "ab"} {
		if _, err := c.Tx(context.Background(), bad); err == nil {
			t.Fatalf("%q was asked for", bad)
		}
	}
	malformed := server(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spent":true,"txid":"../../x"}`))
	})
	defer malformed.Close()
	if _, err := New(malformed.URL).Outspend(context.Background(), txid, 0); err == nil {
		t.Fatal("a malformed spend was accepted")
	}
}

func TestReadsATransaction(t *testing.T) {
	s := server(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"txid":"` + txid + `","vin":[{"txid":"` +
			strings.Repeat("c", 64) + `","vout":2,"witness":["aa"],
			"prevout":{"scriptpubkey_type":"v1_p2tr","scriptpubkey_address":"bc1p","value":7}}],
			"vout":[{"scriptpubkey_address":"bc1q","value":6}],
			"status":{"confirmed":false}}`))
	})
	defer s.Close()
	tx, err := New(s.URL).Tx(context.Background(), txid)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Vin[0].Vout != 2 || tx.Vin[0].Prevout.Value != 7 || tx.Vout[0].Value != 6 {
		t.Fatalf("%+v", tx)
	}
}

func TestReadsAnAddressAndRawTransactions(t *testing.T) {
	s := server(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/address/bc1pexampleexampleexample/txs":
			_, _ = w.Write([]byte(`[{"txid":"` + txid + `","vout":[{"scriptpubkey":"5120ab","scriptpubkey_address":"bc1pexampleexampleexample","value":7}],"status":{"confirmed":true,"block_height":9}}]`))
		case "/tx/" + txid + "/hex":
			_, _ = w.Write([]byte("0200aa\n"))
		default:
			http.NotFound(w, r)
		}
	})
	defer s.Close()
	c := New(s.URL)
	c.Interval = 0
	txs, err := c.AddressTxs(context.Background(), "bc1pexampleexampleexample")
	if err != nil || len(txs) != 1 || txs[0].Vout[0].Value != 7 ||
		txs[0].Vout[0].ScriptPubKey != "5120ab" || txs[0].Status.BlockHeight != 9 {
		t.Fatalf("%+v %v", txs, err)
	}
	raw, err := c.TxHex(context.Background(), txid)
	if err != nil || raw != "0200aa" {
		t.Fatalf("%q %v", raw, err)
	}
	if _, err := c.TxHex(context.Background(), strings.Repeat("b", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown transaction: %v", err)
	}
	if _, err := c.AddressTxs(context.Background(), "../tx/x"); err == nil {
		t.Fatal("a path was taken for an address")
	}
}

func TestReadsTheTip(t *testing.T) {
	s := server(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blocks/tip/height" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("977412"))
	})
	defer s.Close()
	c := New(s.URL)
	c.Interval = 0
	if h, err := c.TipHeight(context.Background()); err != nil || h != 977412 {
		t.Fatalf("%d %v", h, err)
	}
}

func TestFeesAndBroadcast(t *testing.T) {
	var posted []string
	ok := server(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fee-estimates":
			_, _ = w.Write([]byte(`{"1": 20.5, "3": 12, "6": 8.2, "144": 1.1}`))
		case r.Method == "POST" && r.URL.Path == "/tx":
			b, _ := io.ReadAll(r.Body)
			posted = append(posted, string(b))
			_, _ = w.Write([]byte(txid))
		default:
			http.NotFound(w, r)
		}
	})
	defer ok.Close()
	refuses := server(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sendrawtransaction RPC error: bad-txns-inputs-missingorspent", http.StatusBadRequest)
	})
	defer refuses.Close()
	c := New(refuses.URL, ok.URL)
	c.Interval = 0
	if rate, err := c.FeeRate(context.Background(), 5); err != nil || rate != 8.2 {
		t.Fatalf("%v %v", rate, err)
	}
	if _, err := New(ok.URL).FeeRate(context.Background(), 500); err == nil {
		t.Fatal("an estimate past the furthest target")
	}
	got, err := c.Broadcast(context.Background(), "0200aa")
	if err != nil || got != txid || len(posted) != 1 || posted[0] != "0200aa" {
		t.Fatalf("%q %v %v", got, err, posted)
	}
	if _, err := New(refuses.URL).Broadcast(context.Background(), "0200aa"); err == nil ||
		!strings.Contains(err.Error(), "missingorspent") {
		t.Fatalf("refused: %v", err)
	}
}

func TestAddressTxsReadsOlderPages(t *testing.T) {
	page := func(from, n int) string {
		var parts []string
		for i := from; i < from+n; i++ {
			parts = append(parts, fmt.Sprintf(`{"txid":"%064x","status":{"confirmed":true,"block_height":%d}}`, i, 1000-i))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	var paths []string
	s := server(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/address/bc1pexampleexampleexample/txs":
			_, _ = w.Write([]byte(page(0, 25)))
		case fmt.Sprintf("/address/bc1pexampleexampleexample/txs/chain/%064x", 24):
			_, _ = w.Write([]byte(page(25, 25)))
		case fmt.Sprintf("/address/bc1pexampleexampleexample/txs/chain/%064x", 49):
			_, _ = w.Write([]byte(page(50, 3)))
		default:
			http.NotFound(w, r)
		}
	})
	defer s.Close()
	c := New(s.URL)
	c.Interval = 0
	txs, err := c.AddressTxs(context.Background(), "bc1pexampleexampleexample")
	if err != nil || len(txs) != 53 || len(paths) != 3 {
		t.Fatalf("%d txs, %v, paths %v", len(txs), err, paths)
	}
}
