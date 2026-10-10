package esplora

import (
	"context"
	"errors"
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
