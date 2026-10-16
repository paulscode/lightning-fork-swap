package sha256don

import (
	"fmt"
	"time"
)

// Request is one donor's request: an address of ours on both chains, and
// where what reaches it on the BLAKE2b chain goes.
type Request struct {
	ID string `json:"id"`
	// Where BLAKE2b coins go; "" sends them to the swap service's lnd
	// wallet as a liquidity donation
	ReturnAddress string `json:"returnAddress"`
	// Copy the SHA256 donation to the BLAKE2b chain if it isn't there yet
	// (only with a return address, and proofs from its inputs' owners)
	Replay    bool      `json:"replay"`
	CreatedAt time.Time `json:"createdAt"`
	// Given by the worker; "" until then
	Address    string     `json:"address"`
	KeyIndex   int64      `json:"-"`
	LastPolled *time.Time `json:"-"`
}

// Chains
const (
	SHA256  = "sha256"
	BLAKE2b = "blake2b"
)

// Deposit states. On the SHA256 chain: Seen, then Confirmed (kept). On the
// BLAKE2b chain: Seen, Ready (deep enough), Forwarding (in a forward not
// yet confirmed), Forwarded; Kept when too small to pass on; Gone when it
// left the chain before it was deep enough.
const (
	Seen       = "seen"
	Confirmed  = "confirmed"
	Ready      = "ready"
	Forwarding = "forwarding"
	Forwarded  = "forwarded"
	Kept       = "kept"
	Gone       = "gone"
)

// Copy states of a SHA256 deposit whose request asked for a replay
const (
	CopyAlready     = "already"      // it reached the BLAKE2b chain by itself
	CopyDone        = "copied"       // we relayed it there
	CopyNoProof     = "no_proof"     // waiting for its inputs' owners' proofs
	CopyNotPossible = "not_possible" // the BLAKE2b chain refuses it (reason)
)

// Deposit is an output paying a request's address, on either chain.
type Deposit struct {
	ID            int64  `json:"-"`
	RequestID     string `json:"-"`
	Chain         string `json:"chain"`
	Txid          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	ValueSat      int64  `json:"valueSat"`
	Confirmations int64  `json:"confirmations"`
	State         string `json:"state"`
	ForwardID     int64  `json:"-"`
	CopyState     string `json:"copy,omitempty"`
	CopyReason    string `json:"copyReason,omitempty"`
	// Times a BLAKE2b deposit was looked for and not found
	Missing   int       `json:"-"`
	FirstSeen time.Time `json:"firstSeen"`
}

// Outpoint is "txid:vout".
func (d Deposit) Outpoint() string { return fmt.Sprintf("%s:%d", d.Txid, d.Vout) }

// Forward states
const (
	Sent       = "sent"      // relayed, not yet deep enough
	Done       = "confirmed" // deep enough
	FailedSend = "failed"    // never relayed: its coins were released
)

// Forward is a transaction passing BLAKE2b coins on.
type Forward struct {
	ID int64 `json:"-"`
	// "" for a batch to the lnd wallet
	RequestID     string    `json:"-"`
	Txid          string    `json:"txid"`
	Raw           string    `json:"-"`
	Dest          string    `json:"to"`
	AmountSat     int64     `json:"amountSat"`
	FeeSat        int64     `json:"feeSat"`
	State         string    `json:"state"`
	Confirmations int64     `json:"confirmations"`
	Error         string    `json:"-"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Proof is a BIP322 signature by one of a donation's input addresses
// asking for it to be copied (ProofMessage).
type Proof struct {
	Address   string `json:"address"`
	Signature string `json:"signature"`
}

// Hint is a donor telling us their transaction, with proofs for a copy.
type Hint struct {
	ID        int64
	RequestID string
	Txid      string
	Proofs    []Proof
	At        time.Time
}

// ProofMessage is what each input address signs to ask for a copy.
func ProofMessage(requestID, txid, returnAddress string) string {
	return fmt.Sprintf("Lightning Fork Swap: copy %s to the BLAKE2b chain for request %s, return to %s",
		txid, requestID, returnAddress)
}
