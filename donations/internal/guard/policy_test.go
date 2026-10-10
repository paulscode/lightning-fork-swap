package guard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/paulscode/lightning-fork-swap/donations/internal/guard/lnrpc"
)

type leases map[string]bool

func (l leases) LeasedByUs(_ context.Context, txid string, index uint32) (bool, error) {
	if l == nil {
		return false, errors.New("lnd down")
	}
	return l[txid+":"+string(rune('0'+index))], nil
}

var (
	coinA = strings.Repeat("a", 64)
	coinB = strings.Repeat("b", 64)
	peer  = append([]byte{0x02}, make([]byte, 32)...)
)

func policy() Policy {
	p := DefaultPolicy()
	p.Leases = leases{coinA + ":0": true, coinB + ":1": true}
	return p
}

func goodOpen() *lnrpc.OpenChannelRequest {
	return &lnrpc.OpenChannelRequest{
		NodePubkey: peer, LocalFundingAmount: 2_000_000, SatPerVbyte: 3,
		Memo: "donation:AbCdEfGhIjKlMnOpQrSt_-",
		Outpoints: []*lnrpc.OutPoint{
			{TxidStr: coinA, OutputIndex: 0}, {TxidStr: coinB, OutputIndex: 1}},
	}
}

func message(t *testing.T, method, typeName string, m proto.Message) *lnrpc.RPCMessage {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return &lnrpc.RPCMessage{MethodFullUri: method, TypeName: typeName, Serialized: b}
}

func open(t *testing.T, r *lnrpc.OpenChannelRequest) *lnrpc.RPCMessage {
	return message(t, "/lnrpc.Lightning/OpenChannelSync", "lnrpc.OpenChannelRequest", r)
}

func check(p Policy, msg *lnrpc.RPCMessage) error {
	return p.Check(context.Background(), CaveatCondition, msg)
}

func TestAGoodOpenPasses(t *testing.T) {
	if err := check(policy(), open(t, goodOpen())); err != nil {
		t.Fatal(err)
	}
	// fund_max: the whole of the coins, no amount
	r := goodOpen()
	r.FundMax, r.LocalFundingAmount = true, 0
	if err := check(policy(), open(t, r)); err != nil {
		t.Fatal(err)
	}
	// The outpoint by its bytes (reversed) instead of its hex
	r = goodOpen()
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0xaa
	}
	r.Outpoints = []*lnrpc.OutPoint{{TxidBytes: b, OutputIndex: 0}}
	if err := check(policy(), open(t, r)); err != nil {
		t.Fatal(err)
	}
}

func TestEveryDangerousOpenIsRefused(t *testing.T) {
	cases := map[string]func(r *lnrpc.OpenChannelRequest){
		"push to the peer":       func(r *lnrpc.OpenChannelRequest) { r.PushSat = 1 },
		"close to their address": func(r *lnrpc.OpenChannelRequest) { r.CloseAddress = "bc1qthem" },
		"a funding shim":         func(r *lnrpc.OpenChannelRequest) { r.FundingShim = []byte{1} },
		"no outpoints":           func(r *lnrpc.OpenChannelRequest) { r.Outpoints = nil },
		"an unleased coin":       func(r *lnrpc.OpenChannelRequest) { r.Outpoints[1].OutputIndex = 2 },
		"a coin twice":           func(r *lnrpc.OpenChannelRequest) { r.Outpoints[1] = r.Outpoints[0] },
		"a coin in two forms":    func(r *lnrpc.OpenChannelRequest) { r.Outpoints[0].TxidBytes = make([]byte, 32) },
		"a short txid":           func(r *lnrpc.OpenChannelRequest) { r.Outpoints[0].TxidStr = "aa" },
		"unconfirmed coins":      func(r *lnrpc.OpenChannelRequest) { r.SpendUnconfirmed = true },
		"zero conf":              func(r *lnrpc.OpenChannelRequest) { r.ZeroConf = true },
		"an alias":               func(r *lnrpc.OpenChannelRequest) { r.ScidAlias = true },
		"private":                func(r *lnrpc.OpenChannelRequest) { r.Private = true },
		"no fee rate":            func(r *lnrpc.OpenChannelRequest) { r.SatPerVbyte = 0 },
		"fees burned":            func(r *lnrpc.OpenChannelRequest) { r.SatPerVbyte = 101 },
		"a target instead":       func(r *lnrpc.OpenChannelRequest) { r.TargetConf = 1 },
		"the old fee field":      func(r *lnrpc.OpenChannelRequest) { r.SatPerByte = 1 },
		"too big":                func(r *lnrpc.OpenChannelRequest) { r.LocalFundingAmount = 16_777_216 },
		"nothing":                func(r *lnrpc.OpenChannelRequest) { r.LocalFundingAmount = 0 },
		"negative":               func(r *lnrpc.OpenChannelRequest) { r.LocalFundingAmount = -5 },
		"fund_max and an amount": func(r *lnrpc.OpenChannelRequest) { r.FundMax = true },
		"our coins locked long":  func(r *lnrpc.OpenChannelRequest) { r.MaxLocalCsv = 2016 },
		"odd remote delay":       func(r *lnrpc.OpenChannelRequest) { r.RemoteCsvDelay = 9 },
		"a reserve":              func(r *lnrpc.OpenChannelRequest) { r.RemoteChanReserveSat = 1 },
		"a commitment type":      func(r *lnrpc.OpenChannelRequest) { r.CommitmentType = 1 },
		"our fees changed":       func(r *lnrpc.OpenChannelRequest) { r.UseFeeRate, r.FeeRate = true, 5000 },
		"a minimum htlc":         func(r *lnrpc.OpenChannelRequest) { r.MinHtlcMsat = 1 },
		"in-flight limit":        func(r *lnrpc.OpenChannelRequest) { r.RemoteMaxValueInFlightMsat = 1 },
		"htlc limit":             func(r *lnrpc.OpenChannelRequest) { r.RemoteMaxHtlcs = 1 },
		"node by string":         func(r *lnrpc.OpenChannelRequest) { r.NodePubkeyString = "02ab" },
		"no node":                func(r *lnrpc.OpenChannelRequest) { r.NodePubkey = nil },
		"a long memo":            func(r *lnrpc.OpenChannelRequest) { r.Memo = strings.Repeat("x", 501) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := goodOpen()
			mutate(r)
			if err := check(policy(), open(t, r)); !errors.Is(err, ErrRefused) {
				t.Fatalf("passed: %v", err)
			}
		})
	}
	many := goodOpen()
	for i := 0; i < 11; i++ {
		many.Outpoints = append(many.Outpoints, &lnrpc.OutPoint{TxidStr: coinA})
	}
	if check(policy(), open(t, many)) == nil {
		t.Fatal("eleven coins passed")
	}
}

func TestUnknownFieldsAndTypesAreRefused(t *testing.T) {
	msg := open(t, goodOpen())
	// A field this guard does not know, at the top and inside an outpoint
	msg.Serialized = protowire.AppendVarint(protowire.AppendTag(msg.Serialized, 99, protowire.VarintType), 1)
	if err := check(policy(), msg); !errors.Is(err, ErrRefused) {
		t.Fatalf("top-level unknown field passed: %v", err)
	}
	op, _ := proto.Marshal(&lnrpc.OutPoint{TxidStr: coinA})
	op = protowire.AppendVarint(protowire.AppendTag(op, 9, protowire.VarintType), 1)
	b, _ := proto.Marshal(&lnrpc.OpenChannelRequest{NodePubkey: peer, LocalFundingAmount: 1, SatPerVbyte: 1,
		Memo: "donation:AbCdEfGhIjKlMnOpQrSt_-"})
	b = protowire.AppendBytes(protowire.AppendTag(b, 28, protowire.BytesType), op)
	msg = &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/OpenChannelSync",
		TypeName: "lnrpc.OpenChannelRequest", Serialized: b}
	if err := check(policy(), msg); !errors.Is(err, ErrRefused) {
		t.Fatalf("nested unknown field passed: %v", err)
	}
	msg = open(t, goodOpen())
	msg.TypeName = "lnrpc.SendCoinsRequest"
	if check(policy(), msg) == nil {
		t.Fatal("wrong type passed")
	}
	msg = open(t, goodOpen())
	msg.Serialized = []byte{0xff, 0xff}
	if check(policy(), msg) == nil {
		t.Fatal("garbage passed")
	}
}

func TestOnlyListedMethods(t *testing.T) {
	for _, m := range []string{
		"/lnrpc.Lightning/SendCoins", "/lnrpc.Lightning/SendMany",
		"/lnrpc.Lightning/OpenChannel", "/lnrpc.Lightning/CloseChannel",
		"/lnrpc.Lightning/BakeMacaroon", "/routerrpc.Router/SendPaymentV2",
		"/walletrpc.WalletKit/FundPsbt", "/walletrpc.WalletKit/SignPsbt",
		"/walletrpc.WalletKit/PublishTransaction", "/lnrpc.Lightning/RegisterRPCMiddleware",
		"/lnrpc.Lightning/UpdateChannelPolicy", "",
	} {
		if err := check(policy(), &lnrpc.RPCMessage{MethodFullUri: m}); !errors.Is(err, ErrRefused) {
			t.Errorf("%s passed", m)
		}
	}
	for _, m := range []string{"/lnrpc.Lightning/GetInfo", "/walletrpc.WalletKit/ListUnspent",
		"/lnrpc.Lightning/PendingChannels"} {
		if err := check(policy(), &lnrpc.RPCMessage{MethodFullUri: m}); err != nil {
			t.Errorf("%s refused: %v", m, err)
		}
	}
	streamed := &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/GetInfo", StreamRpc: true}
	if check(policy(), streamed) == nil {
		t.Error("a stream passed")
	}
	if policy().Check(context.Background(), "v0", &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/GetInfo"}) == nil {
		t.Error("another caveat condition passed")
	}
}

func TestConnectNewAddressAndLeases(t *testing.T) {
	conn := func(host string, perm bool, timeout uint64) *lnrpc.RPCMessage {
		return message(t, "/lnrpc.Lightning/ConnectPeer", "lnrpc.ConnectPeerRequest",
			&lnrpc.ConnectPeerRequest{Addr: &lnrpc.LightningAddress{
				Pubkey: "02" + strings.Repeat("ab", 32), Host: host}, Perm: perm, Timeout: timeout})
	}
	if err := check(policy(), conn("91.190.100.60:9735", false, 30)); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*lnrpc.RPCMessage{
		conn("127.0.0.1:5432", false, 30), conn("172.30.90.10:9050", false, 30),
		conn("example.com:9735", false, 30), conn("91.190.100.60:9735", true, 30),
		conn("91.190.100.60:9735", false, 600),
	} {
		if check(policy(), m) == nil {
			t.Errorf("connect passed: %v", m)
		}
	}

	addr := func(typ int32, account string) *lnrpc.RPCMessage {
		return message(t, "/lnrpc.Lightning/NewAddress", "lnrpc.NewAddressRequest",
			&lnrpc.NewAddressRequest{Type: typ, Account: account})
	}
	if check(policy(), addr(4, "")) != nil {
		t.Error("a taproot address refused")
	}
	if check(policy(), addr(0, "")) == nil || check(policy(), addr(4, "other")) == nil {
		t.Error("another address kind passed")
	}

	lease := func(id []byte, seconds uint64) *lnrpc.RPCMessage {
		return message(t, "/walletrpc.WalletKit/LeaseOutput", "walletrpc.LeaseOutputRequest",
			&lnrpc.LeaseOutputRequest{Id: id, ExpirationSeconds: seconds,
				Outpoint: &lnrpc.OutPoint{TxidStr: coinA}})
	}
	if check(policy(), lease(LeaseID, 3600)) != nil {
		t.Error("our lease refused")
	}
	if check(policy(), lease(make([]byte, 32), 3600)) == nil ||
		check(policy(), lease(LeaseID, 0)) == nil ||
		check(policy(), lease(LeaseID, 30*24*3600)) == nil {
		t.Error("a bad lease passed")
	}
	release := message(t, "/walletrpc.WalletKit/ReleaseOutput", "walletrpc.ReleaseOutputRequest",
		&lnrpc.ReleaseOutputRequest{Id: []byte("lnd's own lease")})
	if check(policy(), release) == nil {
		t.Error("releasing someone else's lease passed")
	}
}

func TestLeasesThatCannotBeCheckedRefuse(t *testing.T) {
	p := policy()
	p.Leases = leases(nil)
	if check(p, open(t, goodOpen())) == nil {
		t.Fatal("passed without knowing the leases")
	}
	p.Leases = nil
	if check(p, open(t, goodOpen())) == nil {
		t.Fatal("passed without a lease source")
	}
}

func TestAnOpenFromCoinsJustReleased(t *testing.T) {
	p := policy()
	p.Leases = leases{} // nothing leased any more
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	p.Released = NewReleased()
	p.Released.Now = func() time.Time { return now }
	if check(p, open(t, goodOpen())) == nil {
		t.Fatal("an open from coins neither leased nor released passed")
	}
	// The worker releases both: the guard sees the releases pass
	for _, op := range goodOpen().Outpoints {
		msg := message(t, "/walletrpc.WalletKit/ReleaseOutput", "walletrpc.ReleaseOutputRequest",
			&lnrpc.ReleaseOutputRequest{Id: LeaseID, Outpoint: op})
		if err := check(p, msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := check(p, open(t, goodOpen())); err != nil {
		t.Fatalf("refused just after the release: %v", err)
	}
	now = now.Add(3 * time.Minute)
	if check(p, open(t, goodOpen())) == nil {
		t.Fatal("passed long after the release")
	}
}
