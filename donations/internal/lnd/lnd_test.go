package lnd

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestReadsTransactionsWithTheMacaroon(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Grpc-Metadata-macaroon") != "0102" {
			http.Error(w, "no macaroon", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/transactions" || r.URL.Query().Get("start_height") != "100" ||
			r.URL.Query().Get("end_height") != "-1" {
			t.Errorf("request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"transactions":[{"tx_hash":"aa","num_confirmations":2,
			"block_height":120,"output_details":[
			{"address":"bc1pdonate","amount":"5000","is_our_address":true},
			{"address":"bc1qchange","amount":"100","is_our_address":true}]}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.cert")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(certPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	macPath := filepath.Join(dir, "m.macaroon")
	if err := os.WriteFile(macPath, []byte{1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := New(srv.URL, certPath, macPath)
	if err != nil {
		t.Fatal(err)
	}
	txs, err := c.Transactions(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	to, amount := txs[0].Paid(map[string]bool{"bc1pdonate": true})
	if to != "bc1pdonate" || amount != 5000 {
		t.Fatalf("paid %s %d", to, amount)
	}
	if _, amount := txs[0].Paid(map[string]bool{"other": true}); amount != 0 {
		t.Fatal("paid to an address that is not ours")
	}
}

func TestTrustsOnlyItsCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.cert")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE",
		Bytes: other.Certificate().Raw}), 0o600)
	macPath := filepath.Join(dir, "m")
	_ = os.WriteFile(macPath, []byte{1}, 0o600)
	c, err := New(srv.URL, certPath, macPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Transactions(context.Background(), 0); err == nil {
		t.Fatal("a server with another certificate was trusted")
	}
	if _, err := New(srv.URL, macPath, macPath); err == nil {
		t.Fatal("a file without a certificate was accepted")
	}
}
