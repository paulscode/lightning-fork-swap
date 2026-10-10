// Package channels runs channel donations: a donor sends coins to an
// address made for their donation and asks us to use them to open a
// channel from our node to theirs. It is best effort: whatever happens,
// the coins stay with the service (as the channel's balance, or as
// general liquidity when no channel can be opened). Nothing is refunded.
//
// The public API (api.go) creates orders and shows their state; the
// private worker (worker.go) does everything with lnd. They share only
// the database: the API may insert an order and ask for an edit, never
// change one.
package channels

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// State is where an order is.
type State string

// The states, in the order an order usually goes through them; the side
// states after.
const (
	New              State = "new"               // created, no address yet
	AwaitingPayment  State = "awaiting_payment"  // address shown
	PaymentSeen      State = "payment_seen"      // in the mempool or confirming
	PaymentConfirmed State = "payment_confirmed" // enough confirmed, coins leased
	Connecting       State = "connecting"
	Opening          State = "opening"
	FundingBroadcast State = "funding_broadcast" // the channel's transaction is out
	Open             State = "open"              // done
	Retrying         State = "retrying"          // a try failed; another is scheduled
	NeedsAttention   State = "needs_attention"   // the donor can fix it (edit the node)
	FellBack         State = "fell_back"         // kept as a general donation
	Closed           State = "closed"            // the channel was closed later
	Expired          State = "expired"           // nothing received in time
	Rejected         State = "rejected"          // never started (budget)
)

// Final states: nothing more happens to the order (Open still watches
// for a close).
func (s State) Final() bool {
	switch s {
	case FellBack, Closed, Expired, Rejected:
		return true
	}
	return false
}

// Editable states: the donor may still change the node.
func (s State) Editable() bool {
	switch s {
	case New, AwaitingPayment, PaymentSeen, PaymentConfirmed, Retrying, NeedsAttention:
		return true
	}
	return false
}

// Paid states: coins are confirmed and leased for the channel.
func (s State) Paid() bool {
	switch s {
	case PaymentConfirmed, Connecting, Opening, Retrying, NeedsAttention:
		return true
	}
	return false
}

// Error codes shown to the donor (the web app words them); never raw lnd
// text.
const (
	ErrUnreachable    = "unreachable"    // could not connect
	ErrWrongNode      = "wrong_node"     // another key answered there
	ErrNotForkNode    = "not_fork_node"  // not on the Lightning Fork network
	ErrNoAddress      = "no_address"     // no public address known for it
	ErrRejected       = "peer_rejected"  // the node refused the channel
	ErrRejectedSize   = "peer_too_small" // its minimum channel size is larger
	ErrPending        = "peer_pending"   // it has too many channels opening
	ErrDisconnected   = "disconnected"   // it went away while opening
	ErrOurNode        = "our_node"
	ErrNodeLimit      = "node_limit" // it already has its donated channels
	ErrTooLittle      = "too_little"
	ErrBudget         = "budget"
	ErrInternal       = "internal" // our side; we retry
	ErrGaveUp         = "gave_up"  // retried for 48 h
	ErrNoEdit         = "no_edit"  // needed attention for 7 days
	ErrLndUnavailable = "lnd_unavailable"
)

// Rules are the sizes and times of channel donations.
type Rules struct {
	MinChannelSat     int64         // 1,000,000 (Q6); the donation must also pay the fee
	MaxChannelSat     int64         // 16,777,215
	MaxWumboSat       int64         // for nodes that accept large channels
	MaxPerNode        int           // donated channels per node (2)
	MaxUtxos          int           // coins counted per order (10)
	Confirmations     int64         // before opening (3)
	Expiry            time.Duration // to pay (24 h)
	RetryWindow       time.Duration // trying to connect and open (48 h)
	AttentionWindow   time.Duration // waiting for the donor's edit (7 days)
	MaxUnpaid         int           // orders without payment at once (100)
	MaxPerDay         int           // orders created a day (300)
	FeeFloor          uint64        // sat/vB
	FeeCeiling        uint64        // sat/vB, at most the guard's
	LeaseSeconds      uint64
	PruneAfter        time.Duration // orders and events kept (90 days)
	DisclaimerVersion string
}

// DefaultRules are the decided values (Q6, Q7, Q9, Q10).
func DefaultRules() Rules {
	return Rules{
		MinChannelSat: 1_000_000, MaxChannelSat: 16_777_215, MaxWumboSat: 16_777_215,
		MaxPerNode: 2, MaxUtxos: 10, Confirmations: 3,
		Expiry: 24 * time.Hour, RetryWindow: 48 * time.Hour,
		AttentionWindow: 7 * 24 * time.Hour,
		MaxUnpaid:       100, MaxPerDay: 300,
		FeeFloor: 2, FeeCeiling: 100, LeaseSeconds: 14 * 24 * 3600,
		PruneAfter: 90 * 24 * time.Hour, DisclaimerVersion: "2026-10-1",
	}
}

// RetryAfter is when to try again after the nth failure to reach a node:
// 1, 5 and 15 minutes, then hourly.
func RetryAfter(attempts int) time.Duration {
	switch attempts {
	case 0, 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	}
	return time.Hour
}

// Utxo is a coin paid to an order's address.
type Utxo struct {
	Outpoint      string `json:"outpoint"`
	AmountSat     int64  `json:"amountSat"`
	Confirmations int64  `json:"confirmations"`
	Leased        bool   `json:"leased"`
}

// Order is one channel donation.
type Order struct {
	ID                string
	SecretHash        string
	NodePubkey        string
	NodeAddr          string // the address to dial; "" = from the graph
	NodeInput         string // what the donor typed for the address
	NodeAlias         string
	State             State
	Address           string
	DisclaimerVersion string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	UpdatedAt         time.Time
	ReceivedSat       int64
	ConfirmedSat      int64
	Utxos             []Utxo
	Wumbo             bool
	ChannelPoint      string
	CapacitySat       int64
	FeeSat            int64
	RemainderSat      int64
	FundingConfs      int64
	Attempts          int
	NextAttempt       *time.Time
	ErrorCode         string
	AttentionSince    *time.Time
	FailingSince      *time.Time
	OpenedAt          *time.Time
	ClosedAt          *time.Time
	Version           int64
}

// Event is one step of an order's timeline, as the donor sees it.
type Event struct {
	OrderID string         `json:"-"`
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// Request is the API's ask of the worker (an edit of the node).
type Request struct {
	ID      int64
	OrderID string
	Kind    string
	Node    string // pubkey
	Addr    string // resolved address, or ""
	Input   string
}

// NewID is a random 128-bit order id.
func NewID() string { return random(16) }

// NewSecret is the donor's secret for edits, and its hash (stored).
func NewSecret() (secret, hash string) {
	secret = random(32)
	return secret, HashSecret(secret)
}

// HashSecret is what is stored for a secret.
func HashSecret(secret string) string {
	h := sha256.Sum256([]byte("lfswap channel donation secret:" + secret))
	return hex.EncodeToString(h[:])
}

// SecretMatches compares in constant time.
func SecretMatches(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(hash)) == 1
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ValidID reports whether s looks like an order id (22 base64url chars).
func ValidID(s string) bool {
	if len(s) != 22 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}
