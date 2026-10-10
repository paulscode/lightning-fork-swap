package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

// The BIP-322 test vector: this address signed "Hello World"
const (
	signer    = "bc1q9vza2e8x573nczrlzms0wvx3gsqjx7vavgkx0l"
	signature = "AkcwRAIgZRfIY3p7/DoVTty6YZbWS71bc5Vct9p9Fia83eRmw2QCICK/ENGfwLtptFluMGs2KsqoNSk89pO7F29zJLUx9a/sASECx/EgAxlkQpQ9hYjgGu6EBCPMVPwVIVJqO4XCsMvViHI="
)

func record(addresses ...string) store.Record {
	r := store.Record{Txid: strings.Repeat("d", 64)}
	for _, a := range addresses {
		r.Inputs = append(r.Inputs, replay.Input{Address: a})
	}
	return r
}

func TestVerifyOwnership(t *testing.T) {
	r := record("bc1qsomeoneelse", signer)
	if err := VerifyOwnership(r, signer, "Hello World", signature); err != nil {
		t.Fatalf("a valid proof refused: %v", err)
	}
	// A valid signature by an address that did not fund the donation
	if err := VerifyOwnership(record("bc1qsomeoneelse"), signer, "Hello World", signature); err == nil ||
		!strings.Contains(err.Error(), "not among the inputs") {
		t.Fatalf("an outsider's proof accepted: %v", err)
	}
	// The right address, another message
	if err := VerifyOwnership(r, signer, "Send it to me", signature); err == nil {
		t.Fatal("a signature over another message accepted")
	}
}

func TestUsage(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	for _, args := range [][]string{
		nil,
		{"nonsense"},
		{"check"},
		{"replay"},
	} {
		if err := run(context.Background(), args, logger); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestList(t *testing.T) {
	t.Setenv("X_LIST", " https://a/api/, ,https://b/api ")
	got := list("X_LIST", "https://fallback")
	if strings.Join(got, "|") != "https://a/api|https://b/api" {
		t.Fatalf("%q", got)
	}
	t.Setenv("X_LIST", "")
	if got := list("X_LIST", "https://fallback/"); len(got) != 1 || got[0] != "https://fallback" {
		t.Fatalf("%q", got)
	}
}
