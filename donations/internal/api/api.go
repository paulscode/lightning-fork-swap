// Package api answers the web app's questions about donations. It reads
// the database only; it holds no macaroon and talks to nothing else.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
	"github.com/paulscode/lightning-fork-swap/donations/internal/replay"
	"github.com/paulscode/lightning-fork-swap/donations/internal/store"
)

// Routes is more of /donate/v1/ (the channel donations).
type Routes interface{ Register(mux *http.ServeMux) }

// Handler serves /donate/v1/.
func Handler(s store.Store, more ...Routes) http.Handler {
	mux := http.NewServeMux()
	for _, r := range more {
		r.Register(mux)
	}
	mux.HandleFunc("GET /donate/v1/health", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /donate/v1/replay/{txid}", func(w http.ResponseWriter, r *http.Request) {
		txid := strings.ToLower(r.PathValue("txid"))
		if !esplora.ValidTxid(txid) {
			write(w, http.StatusBadRequest, map[string]string{"error": "not a txid"})
			return
		}
		rec, err := s.Get(r.Context(), txid)
		if errors.Is(err, store.ErrNotFound) {
			write(w, http.StatusNotFound, map[string]string{"error": "not a donation"})
			return
		}
		if err != nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		// The donor's own addresses, and only when they need to act on them
		addresses := []string{}
		if rec.Verdict == replay.AtRisk || rec.Verdict == replay.Replayed {
			addresses = rec.AtRiskAddresses
		}
		write(w, http.StatusOK, map[string]any{
			"verdict":         rec.Verdict,
			"atRiskAddresses": addresses,
		})
	})
	return mux
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
