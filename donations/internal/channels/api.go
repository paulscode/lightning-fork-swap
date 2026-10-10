package channels

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	"github.com/paulscode/lightning-fork-swap/donations/internal/netcheck"
)

// API serves the channel donation routes under /donate/v1/.
type API struct {
	Store    Store
	Rules    Rules
	PoW      *PoW
	Resolver netcheck.Resolver
	Now      func() time.Time
	// How long POST waits for the worker to hand out the address
	AddressWait time.Duration
}

// Register adds the routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /donate/v1/info", a.info)
	mux.HandleFunc("POST /donate/v1/channel-orders", a.create)
	mux.HandleFunc("GET /donate/v1/channel-orders/{id}", a.get)
	mux.HandleFunc("PATCH /donate/v1/channel-orders/{id}/node", a.edit)
}

// What the API says when it refuses; the web app words each code
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

// available: the worker passed recently, so an order will be served
func (a *API) available(ctx context.Context) (bool, uint64, int64) {
	last, err := a.Store.Setting(ctx, SettingLastPass)
	if err != nil || last == "" {
		return false, 0, 0
	}
	at, err := time.Parse(time.RFC3339, last)
	if err != nil || a.Now().Sub(at) > 5*time.Minute {
		return false, 0, 0
	}
	rateText, _ := a.Store.Setting(ctx, SettingFeeRate)
	minText, _ := a.Store.Setting(ctx, SettingMinSat)
	rate, _ := strconv.ParseUint(rateText, 10, 64)
	min, _ := strconv.ParseInt(minText, 10, 64)
	if min == 0 {
		min = a.Rules.MinDonation(a.Rules.FeeFloor)
	}
	return true, rate, min
}

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	ok, rate, min := a.available(r.Context())
	body := map[string]any{
		"available":         ok,
		"minSat":            min,
		"maxSat":            a.Rules.MaxChannelSat,
		"maxWumboSat":       a.Rules.MaxWumboSat,
		"feeRate":           rate,
		"confirmations":     a.Rules.Confirmations,
		"expiryHours":       int(a.Rules.Expiry.Hours()),
		"maxPerNode":        a.Rules.MaxPerNode,
		"disclaimerVersion": a.Rules.DisclaimerVersion,
		"powBits":           a.PoW.Bits,
		"challenge":         a.PoW.Challenge(a.Now()),
	}
	if ok {
		if unpaid, today, err := a.Store.Counts(r.Context(), a.Now()); err == nil {
			body["openSlots"] = max(0, min64(int64(a.Rules.MaxUnpaid-unpaid),
				int64(a.Rules.MaxPerDay-today)))
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// node reads what the donor typed: pubkey, pubkey@host or pubkey@host:port.
// The host is resolved once, here; without one the worker uses the graph.
func (a *API) node(ctx context.Context, input string) (pubkey, addr, shown string, code string) {
	input = strings.TrimSpace(input)
	if len(input) > 400 {
		return "", "", "", "bad_node"
	}
	key, host, _ := strings.Cut(input, "@")
	key = strings.ToLower(key)
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) != 33 {
		return "", "", "", "bad_node"
	}
	if _, err := btcec.ParsePubKey(raw); err != nil {
		return "", "", "", "bad_node"
	}
	if host == "" {
		return key, "", "", ""
	}
	addr, err = netcheck.Resolve(ctx, a.Resolver, host, 9735)
	if err != nil {
		if errors.Is(err, netcheck.ErrNotPublic) {
			return "", "", "", "not_public"
		}
		return "", "", "", "bad_host"
	}
	return key, addr, host, ""
}

type createRequest struct {
	Node              string `json:"node"`
	Accepted          bool   `json:"accepted"`
	DisclaimerVersion string `json:"disclaimerVersion"`
	Challenge         string `json:"challenge"`
	Nonce             string `json:"nonce"`
}

func readJSON(r *http.Request, into any) error {
	body := io.LimitReader(r.Body, 4096)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// checkNode refuses our own node and nodes that have their donated channels
func (a *API) checkNode(ctx context.Context, pubkey string) (int, string, string) {
	if ours, _ := a.Store.Setting(ctx, SettingOurNode); ours != "" && ours == pubkey {
		return http.StatusUnprocessableEntity, ErrOurNode, "that is our node"
	}
	n, err := a.Store.ForNode(ctx, pubkey)
	if err != nil {
		return http.StatusServiceUnavailable, "unavailable", "try again later"
	}
	if n >= a.Rules.MaxPerNode {
		return http.StatusUnprocessableEntity, ErrNodeLimit, "this node already has its donated channels"
	}
	return 0, "", ""
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createRequest
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "cannot read the request")
		return
	}
	if !req.Accepted || req.DisclaimerVersion != a.Rules.DisclaimerVersion {
		fail(w, http.StatusConflict, "disclaimer", "the terms of this donation must be accepted as shown")
		return
	}
	if err := a.PoW.Verify(req.Challenge, req.Nonce, a.Now()); err != nil {
		fail(w, http.StatusBadRequest, "work", err.Error())
		return
	}
	ok, _, _ := a.available(ctx)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "unavailable", "channel donations are paused")
		return
	}
	pubkey, addr, shown, code := a.node(ctx, req.Node)
	if code != "" {
		fail(w, http.StatusUnprocessableEntity, code, "check the node's key and address")
		return
	}
	if status, code, text := a.checkNode(ctx, pubkey); status != 0 {
		fail(w, status, code, text)
		return
	}
	unpaid, today, err := a.Store.Counts(ctx, a.Now())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	if unpaid >= a.Rules.MaxUnpaid || today >= a.Rules.MaxPerDay {
		fail(w, http.StatusServiceUnavailable, ErrBudget, "too many donations are waiting; try again later")
		return
	}
	secret, hash := NewSecret()
	o := Order{ID: NewID(), SecretHash: hash, NodePubkey: pubkey, NodeAddr: addr,
		NodeInput: shown, DisclaimerVersion: a.Rules.DisclaimerVersion,
		ExpiresAt: a.Now().Add(a.Rules.Expiry)}
	if err := a.Store.CreateOrder(ctx, o); err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	// The worker hands out the address in a moment
	deadline := time.Now().Add(a.AddressWait)
	got, err := a.Store.GetOrder(ctx, o.ID)
	for err == nil && got.State == New && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		got, err = a.Store.GetOrder(ctx, o.ID)
	}
	if err != nil {
		got = o
	}
	view := a.view(ctx, got)
	writeJSON(w, http.StatusCreated, map[string]any{"id": o.ID, "secret": secret, "order": view})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidID(id) {
		fail(w, http.StatusNotFound, "not_found", "no such donation")
		return
	}
	o, err := a.Store.GetOrder(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "no such donation")
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, o.ID[:8], o.Version)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, a.view(r.Context(), o))
}

type editRequest struct {
	Secret string `json:"secret"`
	Node   string `json:"node"`
}

func (a *API) edit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var req editRequest
	if !ValidID(id) || readJSON(r, &req) != nil {
		fail(w, http.StatusBadRequest, "bad_request", "cannot read the request")
		return
	}
	o, err := a.Store.GetOrder(ctx, id)
	// The same answer for a missing order and a wrong secret
	if err != nil || !SecretMatches(req.Secret, o.SecretHash) {
		fail(w, http.StatusForbidden, "forbidden", "not this donation's secret")
		return
	}
	if !o.State.Editable() {
		fail(w, http.StatusConflict, "not_editable", "the node can no longer be changed")
		return
	}
	pubkey, addr, shown, code := a.node(ctx, req.Node)
	if code != "" {
		fail(w, http.StatusUnprocessableEntity, code, "check the node's key and address")
		return
	}
	if pubkey != o.NodePubkey {
		if status, code, text := a.checkNode(ctx, pubkey); status != 0 {
			fail(w, status, code, text)
			return
		}
	}
	if err := a.Store.AddRequest(ctx, Request{OrderID: id, Kind: "node", Node: pubkey,
		Addr: addr, Input: shown}); err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "try again later")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// Detail keys a donor may see in the timeline
var publicDetail = map[string]bool{
	"code": true, "receivedSat": true, "confirmedSat": true, "capacitySat": true,
	"remainderSat": true, "channelPoint": true, "confirmations": true, "next": true,
	"closeType": true, "address": true,
}

// view is an order as the donor sees it.
func (a *API) view(ctx context.Context, o Order) map[string]any {
	_, _, min := a.available(ctx)
	events, _ := a.Store.Events(ctx, o.ID)
	timeline := []map[string]any{}
	for _, e := range events {
		item := map[string]any{"at": e.At, "kind": e.Kind}
		detail := map[string]any{}
		for k, v := range e.Detail {
			if publicDetail[k] {
				detail[k] = v
			}
		}
		if len(detail) > 0 {
			item["detail"] = detail
		}
		timeline = append(timeline, item)
	}
	max := a.Rules.MaxChannelSat
	if o.Wumbo && a.Rules.MaxWumboSat > max {
		max = a.Rules.MaxWumboSat
	}
	view := map[string]any{
		"id": o.ID, "state": o.State, "address": o.Address,
		"node":   map[string]any{"pubkey": o.NodePubkey, "alias": o.NodeAlias, "address": o.NodeInput},
		"minSat": min, "maxSat": max,
		"receivedSat": o.ReceivedSat, "confirmedSat": o.ConfirmedSat,
		"confirmationsNeeded": a.Rules.Confirmations,
		"createdAt":           o.CreatedAt, "expiresAt": o.ExpiresAt,
		"capacitySat": o.CapacitySat, "remainderSat": o.RemainderSat, "feeSat": o.FeeSat,
		"fundingConfirmations": o.FundingConfs,
		"editable":             o.State.Editable(),
		"disclaimerVersion":    o.DisclaimerVersion,
		"timeline":             timeline,
	}
	if o.ChannelPoint != "" {
		view["channelPoint"] = o.ChannelPoint
	}
	if o.ErrorCode != "" {
		view["errorCode"] = o.ErrorCode
	}
	if o.NextAttempt != nil {
		view["nextAttempt"] = o.NextAttempt
	}
	for k, t := range map[string]*time.Time{"openedAt": o.OpenedAt, "closedAt": o.ClosedAt} {
		if t != nil {
			view[k] = t
		}
	}
	return view
}
