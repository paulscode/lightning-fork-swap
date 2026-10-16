package sha256don

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"

	"github.com/paulscode/lightning-fork-swap/donations/internal/bip322"
	"github.com/paulscode/lightning-fork-swap/donations/internal/channels"
	"github.com/paulscode/lightning-fork-swap/donations/internal/esplora"
)

// API serves /donate/v1/sha256: permissionless; it holds no key, and may
// only insert a request's first columns and a hint.
type API struct {
	Store  Store
	PoW    *channels.PoW
	Params *chaincfg.Params
	Now    func() time.Time
	// Requests a day, all together
	MaxPerDay int
	// How long POST waits for the worker to hand out the address
	AddressWait time.Duration
	// Depths, as the worker counts them (shown to callers)
	Sha256Confs, Blake2bConfs int64
	// Checks a proof; nil is BIP322
	Verify func(address, message, signature string) error
}

// Register adds the routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /donate/v1/sha256/info", a.info)
	mux.HandleFunc("POST /donate/v1/sha256", a.create)
	mux.HandleFunc("GET /donate/v1/sha256/{id}", a.get)
	mux.HandleFunc("POST /donate/v1/sha256/{id}/tx", a.hint)
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func fail(w http.ResponseWriter, status int, code, text string) {
	writeJSON(w, status, apiError{Error: text, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func readJSON(r *http.Request, into any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// available: the worker passed recently, so a request will be served
func (a *API) available(r *http.Request) bool {
	last, err := a.Store.Setting(r.Context(), SettingLastPass)
	if err != nil || last == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, last)
	return err == nil && a.Now().Sub(at) < 5*time.Minute
}

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"available":     a.available(r),
		"powBits":       a.PoW.Bits,
		"challenge":     a.PoW.Challenge(a.Now()),
		"minForwardSat": MinForwardSat,
		"confirmations": map[string]int64{SHA256: a.Sha256Confs, BLAKE2b: a.Blake2bConfs},
		"proofMessage":  ProofMessage("<id>", "<txid>", "<returnAddress>"),
	})
}

type createRequest struct {
	ReturnAddress string `json:"returnAddress"`
	Replay        bool   `json:"replay"`
	Challenge     string `json:"challenge"`
	Nonce         string `json:"nonce"`
}

// checkAddress accepts a mainnet address, in its usual form.
func (a *API) checkAddress(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 90 {
		return "", false
	}
	addr, err := btcutil.DecodeAddress(s, a.Params)
	if err != nil || !addr.IsForNet(a.Params) {
		return "", false
	}
	return addr.EncodeAddress(), true
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "cannot read the request")
		return
	}
	if err := a.PoW.Verify(req.Challenge, req.Nonce, a.Now()); err != nil {
		fail(w, http.StatusBadRequest, "work", err.Error())
		return
	}
	ret := ""
	if strings.TrimSpace(req.ReturnAddress) != "" {
		var ok bool
		if ret, ok = a.checkAddress(req.ReturnAddress); !ok {
			fail(w, http.StatusUnprocessableEntity, "bad_address", "not a mainnet address")
			return
		}
	}
	if req.Replay && ret == "" {
		fail(w, http.StatusUnprocessableEntity, "replay_needs_address", "a copy needs a return address")
		return
	}
	if !a.available(r) {
		fail(w, http.StatusServiceUnavailable, "unavailable", "SHA256 donations are paused")
		return
	}
	n, err := a.Store.CreatedSince(ctx, a.Now().Add(-24*time.Hour))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	if n >= a.MaxPerDay {
		fail(w, http.StatusServiceUnavailable, "budget", "too many requests today; try again later")
		return
	}
	req0 := Request{ID: channels.NewID(), ReturnAddress: ret, Replay: req.Replay}
	if err := a.Store.CreateRequest(ctx, req0); err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	// The worker hands out the address in a moment
	deadline := time.Now().Add(a.AddressWait)
	got, err := a.Store.GetRequest(ctx, req0.ID)
	for err == nil && got.Address == "" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		got, err = a.Store.GetRequest(ctx, req0.ID)
	}
	if err != nil {
		got = req0
	}
	view, err := a.view(r, got)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// View is a request as callers see it.
type View struct {
	Request
	// Pending: the worker hasn't given the address yet; ask again
	Pending  bool      `json:"pending"`
	Sha256   []Deposit `json:"sha256"`
	Blake2b  []Deposit `json:"blake2b"`
	Forwards []Forward `json:"forwards"`
}

func (a *API) view(r *http.Request, req Request) (View, error) {
	v := View{Request: req, Pending: req.Address == "", Sha256: []Deposit{},
		Blake2b: []Deposit{}, Forwards: []Forward{}}
	deposits, err := a.Store.Deposits(r.Context(), req.ID)
	if err != nil {
		return v, err
	}
	forwards, err := a.Store.Forwards(r.Context(), req.ID)
	if err != nil {
		return v, err
	}
	v.Forwards = append(v.Forwards, forwards...)
	txids := map[int64]string{}
	for _, f := range forwards {
		txids[f.ID] = f.Txid
	}
	for _, d := range deposits {
		if d.Chain == SHA256 {
			v.Sha256 = append(v.Sha256, d)
		} else {
			d.ForwardTxid = txids[d.ForwardID]
			v.Blake2b = append(v.Blake2b, d)
		}
	}
	return v, nil
}

func (a *API) request(w http.ResponseWriter, r *http.Request) (Request, bool) {
	id := r.PathValue("id")
	if !channels.ValidID(id) {
		fail(w, http.StatusNotFound, "not_found", "no such request")
		return Request{}, false
	}
	req, err := a.Store.GetRequest(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "no such request")
		return Request{}, false
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return Request{}, false
	}
	return req, true
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	req, ok := a.request(w, r)
	if !ok {
		return
	}
	view, err := a.view(r, req)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type hintRequest struct {
	Txid   string  `json:"txid"`
	Proofs []Proof `json:"proofs"`
}

// MaxProofs is the most proofs one hint carries.
const MaxProofs = 20

// MaxHints is the most hints a request keeps.
const MaxHints = 20

func (a *API) hint(w http.ResponseWriter, r *http.Request) {
	req, ok := a.request(w, r)
	if !ok {
		return
	}
	var h hintRequest
	if err := readJSON(r, &h); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "cannot read the request")
		return
	}
	if !esplora.ValidTxid(h.Txid) {
		fail(w, http.StatusUnprocessableEntity, "bad_txid", "not a transaction id")
		return
	}
	if len(h.Proofs) > MaxProofs {
		fail(w, http.StatusUnprocessableEntity, "too_many_proofs", "too many proofs")
		return
	}
	if len(h.Proofs) > 0 && !req.Replay {
		fail(w, http.StatusUnprocessableEntity, "no_replay", "this request did not ask for a copy")
		return
	}
	message := ProofMessage(req.ID, h.Txid, req.ReturnAddress)
	for _, p := range h.Proofs {
		verify := a.Verify
		if verify == nil {
			verify = func(address, message, signature string) error {
				return bip322.Verify(address, message, signature, a.Params)
			}
		}
		if len(p.Address) > 90 || len(p.Signature) > 2000 ||
			verify(p.Address, message, p.Signature) != nil {
			fail(w, http.StatusUnprocessableEntity, "bad_proof",
				"a proof does not sign the message for its address: "+p.Address)
			return
		}
	}
	hints, err := a.Store.Hints(r.Context(), req.ID, "")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	if len(hints) >= MaxHints {
		fail(w, http.StatusUnprocessableEntity, "too_many_hints", "this request has enough hints")
		return
	}
	if err := a.Store.AddHint(r.Context(), Hint{RequestID: req.ID, Txid: h.Txid,
		Proofs: h.Proofs}); err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}
