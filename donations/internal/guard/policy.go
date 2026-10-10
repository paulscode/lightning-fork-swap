// Package guard is the hard limit on what the channel donation worker can
// make lnd do. lnd hands every request made with the worker's macaroon
// (which carries the custom caveat CaveatName) to this middleware before
// running it, and refuses such requests outright while no middleware for
// the caveat is registered. The guard allows only the calls the worker
// needs, and opens only channels funded from the worker's leased coins,
// with nothing pushed to the peer and no address of theirs to close to.
package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/paulscode/lightning-fork-swap/donations/internal/guard/lnrpc"
	"github.com/paulscode/lightning-fork-swap/donations/internal/netcheck"
)

const (
	// CaveatName is the custom macaroon caveat the guard answers for
	CaveatName = "lfswap-donations"
	// CaveatCondition is the condition baked into the worker's macaroon
	CaveatCondition = "v1"
)

// LeaseID is the id the worker leases donated coins under: only coins
// leased with it may fund a donated channel.
var LeaseID = func() []byte {
	h := sha256.Sum256([]byte("lightning-fork-swap channel donations"))
	return h[:]
}()

// Methods the worker may call; those with a check get it, the rest are
// reads. Anything else is refused.
var allowed = map[string]bool{
	"/lnrpc.Lightning/GetInfo":           true,
	"/lnrpc.Lightning/GetNodeInfo":       true,
	"/lnrpc.Lightning/ListPeers":         true,
	"/lnrpc.Lightning/ListChannels":      true,
	"/lnrpc.Lightning/PendingChannels":   true,
	"/lnrpc.Lightning/ClosedChannels":    true,
	"/lnrpc.Lightning/GetTransactions":   true,
	"/walletrpc.WalletKit/ListUnspent":   true,
	"/walletrpc.WalletKit/ListLeases":    true,
	"/walletrpc.WalletKit/EstimateFee":   true,
	"/lnrpc.Lightning/NewAddress":        true,
	"/walletrpc.WalletKit/LeaseOutput":   true,
	"/walletrpc.WalletKit/ReleaseOutput": true,
	"/lnrpc.Lightning/ConnectPeer":       true,
	"/lnrpc.Lightning/OpenChannelSync":   true,
}

// Leases tells whether an outpoint is leased under LeaseID right now.
type Leases interface {
	LeasedByUs(ctx context.Context, txid string, index uint32) (bool, error)
}

// Policy is what the guard allows.
type Policy struct {
	// The largest channel the worker may open, in sat
	MaxChannelSat int64
	// The highest fee rate it may pay, in sat/vB
	MaxFeeRate uint64
	// At most this many coins fund one channel
	MaxOutpoints int
	// The longest a lease may last, in seconds
	MaxLeaseSeconds uint64
	Leases          Leases
}

// DefaultPolicy is the production limit, before the leases source is set.
func DefaultPolicy() Policy {
	return Policy{MaxChannelSat: 16_777_215, MaxFeeRate: 100, MaxOutpoints: 10,
		MaxLeaseSeconds: 14 * 24 * 3600}
}

// ErrRefused prefixes every refusal.
var ErrRefused = errors.New("refused by the donations guard")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Check judges one intercepted request.
func (p Policy) Check(ctx context.Context, condition string, msg *lnrpc.RPCMessage) error {
	if condition != CaveatCondition {
		return refuse("unknown caveat condition %q", condition)
	}
	if msg == nil {
		return refuse("no request")
	}
	method := msg.MethodFullUri
	if !allowed[method] {
		return refuse("%s is not allowed", method)
	}
	if msg.StreamRpc {
		return refuse("no streaming calls")
	}
	switch method {
	case "/lnrpc.Lightning/OpenChannelSync":
		var req lnrpc.OpenChannelRequest
		if err := decode(msg, "lnrpc.OpenChannelRequest", &req); err != nil {
			return err
		}
		return p.openChannel(ctx, &req)
	case "/lnrpc.Lightning/ConnectPeer":
		var req lnrpc.ConnectPeerRequest
		if err := decode(msg, "lnrpc.ConnectPeerRequest", &req); err != nil {
			return err
		}
		return connectPeer(&req)
	case "/lnrpc.Lightning/NewAddress":
		var req lnrpc.NewAddressRequest
		if err := decode(msg, "lnrpc.NewAddressRequest", &req); err != nil {
			return err
		}
		// A taproot address of the default account
		if req.Type != 4 || (req.Account != "" && req.Account != "default") {
			return refuse("only taproot addresses of the default account")
		}
	case "/walletrpc.WalletKit/LeaseOutput":
		var req lnrpc.LeaseOutputRequest
		if err := decode(msg, "walletrpc.LeaseOutputRequest", &req); err != nil {
			return err
		}
		if string(req.Id) != string(LeaseID) {
			return refuse("leases only under the donations lease id")
		}
		if req.ExpirationSeconds == 0 || req.ExpirationSeconds > p.MaxLeaseSeconds {
			return refuse("lease of %d s", req.ExpirationSeconds)
		}
	case "/walletrpc.WalletKit/ReleaseOutput":
		var req lnrpc.ReleaseOutputRequest
		if err := decode(msg, "walletrpc.ReleaseOutputRequest", &req); err != nil {
			return err
		}
		if string(req.Id) != string(LeaseID) {
			return refuse("releases only under the donations lease id")
		}
	}
	return nil
}

func (p Policy) openChannel(ctx context.Context, r *lnrpc.OpenChannelRequest) error {
	switch {
	case r.PushSat != 0:
		return refuse("push_sat must be 0")
	case r.CloseAddress != "":
		return refuse("no close address")
	case len(r.FundingShim) != 0:
		return refuse("no funding shim")
	case len(r.Outpoints) == 0 || len(r.Outpoints) > p.MaxOutpoints:
		return refuse("%d outpoints", len(r.Outpoints))
	case r.SpendUnconfirmed:
		return refuse("confirmed coins only")
	case r.ZeroConf || r.ScidAlias || r.Private:
		return refuse("public, ordinary channels only")
	case r.SatPerVbyte == 0 || r.SatPerVbyte > p.MaxFeeRate || r.SatPerByte != 0 ||
		r.TargetConf != 0:
		return refuse("fee rate %d sat/vB", r.SatPerVbyte)
	case r.FundMax && r.LocalFundingAmount != 0:
		return refuse("fund_max with an amount")
	case !r.FundMax && (r.LocalFundingAmount <= 0 || r.LocalFundingAmount > p.MaxChannelSat):
		return refuse("channel of %d sat", r.LocalFundingAmount)
	case r.MinHtlcMsat != 0 || r.RemoteCsvDelay != 0 || r.RemoteMaxValueInFlightMsat != 0 ||
		r.RemoteMaxHtlcs != 0 || r.MaxLocalCsv != 0 || r.CommitmentType != 0 ||
		r.RemoteChanReserveSat != 0 || r.UseBaseFee || r.UseFeeRate ||
		r.BaseFee != 0 || r.FeeRate != 0:
		return refuse("channel parameters must be lnd's defaults")
	case len(r.NodePubkey) != 33 || r.NodePubkeyString != "":
		return refuse("node by its 33-byte key only")
	case len(r.Memo) > 500:
		return refuse("memo too long")
	}
	seen := map[string]bool{}
	for _, op := range r.Outpoints {
		txid := op.TxidStr
		if txid == "" && len(op.TxidBytes) == 32 {
			// lnd's byte order is reversed from the hex form
			b := make([]byte, 32)
			for i := range b {
				b[i] = op.TxidBytes[31-i]
			}
			txid = hex.EncodeToString(b)
		} else if len(op.TxidBytes) != 0 {
			return refuse("an outpoint in two forms")
		}
		txid = strings.ToLower(txid)
		if len(txid) != 64 {
			return refuse("bad outpoint")
		}
		key := fmt.Sprintf("%s:%d", txid, op.OutputIndex)
		if seen[key] {
			return refuse("an outpoint twice")
		}
		seen[key] = true
		if p.Leases == nil {
			return refuse("cannot check leases")
		}
		ok, err := p.Leases.LeasedByUs(ctx, txid, op.OutputIndex)
		if err != nil {
			return refuse("cannot check leases: %v", err)
		}
		if !ok {
			return refuse("%s is not leased for donations", key)
		}
	}
	return nil
}

func connectPeer(r *lnrpc.ConnectPeerRequest) error {
	if r.Addr == nil || r.Perm || r.Timeout > 60 {
		return refuse("connect: address, not permanent, at most 60 s")
	}
	if len(r.Addr.Pubkey) != 66 {
		return refuse("connect: bad key")
	}
	if _, err := hex.DecodeString(r.Addr.Pubkey); err != nil {
		return refuse("connect: bad key")
	}
	if _, err := netcheck.Literal(r.Addr.Host); err != nil {
		return refuse("connect: %s is not a public address", r.Addr.Host)
	}
	return nil
}

// decode reads the request, refusing a type it did not expect and any
// field it does not know (one could do something the checks miss).
func decode(msg *lnrpc.RPCMessage, typeName string, into proto.Message) error {
	if msg.TypeName != typeName {
		return refuse("unexpected type %s", msg.TypeName)
	}
	if err := proto.Unmarshal(msg.Serialized, into); err != nil {
		return refuse("unreadable request")
	}
	if unknown(into.ProtoReflect()) {
		return refuse("unknown fields in %s", typeName)
	}
	return nil
}

func unknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if unknown(list.Get(i).Message()) {
					found = true
				}
			}
		case fd.Message() != nil && !fd.IsMap():
			if unknown(v.Message()) {
				found = true
			}
		}
		return !found
	})
	return found
}

// OpenChannelCheck judges an open request on its own (the worker's tests
// use it to show the worker asks only for what the guard allows).
func (p Policy) OpenChannelCheck(ctx context.Context, r *lnrpc.OpenChannelRequest) error {
	return p.openChannel(ctx, r)
}
