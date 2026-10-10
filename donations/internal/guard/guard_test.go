package guard

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/paulscode/lightning-fork-swap/donations/internal/guard/lnrpc"
)

// fakeLnd plays lnd's side of RegisterRPCMiddleware: it checks the
// registration, then sends the interceptions in `send` and collects the
// guard's feedback.
type fakeLnd struct {
	send     []*lnrpc.RPCMiddlewareRequest
	reg      *lnrpc.MiddlewareRegistration
	feedback map[uint64]*lnrpc.InterceptFeedback
	done     chan struct{}
}

func (f *fakeLnd) handle(_ any, stream grpc.ServerStream) error {
	var first lnrpc.RPCMiddlewareResponse
	if err := stream.RecvMsg(&first); err != nil {
		return err
	}
	f.reg = first.GetRegister()
	if err := stream.SendMsg(&lnrpc.RPCMiddlewareRequest{
		InterceptType: &lnrpc.RPCMiddlewareRequest_RegComplete{RegComplete: true}}); err != nil {
		return err
	}
	for _, req := range f.send {
		if err := stream.SendMsg(req); err != nil {
			return err
		}
		var res lnrpc.RPCMiddlewareResponse
		if err := stream.RecvMsg(&res); err != nil {
			return err
		}
		f.feedback[res.RefMsgId] = res.GetFeedback()
	}
	close(f.done)
	<-stream.Context().Done()
	return nil
}

func TestServeSpeaksLndsProtocol(t *testing.T) {
	good := open(t, goodOpen())
	bad := goodOpen()
	bad.PushSat = 5
	f := &fakeLnd{
		feedback: map[uint64]*lnrpc.InterceptFeedback{},
		done:     make(chan struct{}),
		send: []*lnrpc.RPCMiddlewareRequest{
			{MsgId: 1, CustomCaveatCondition: CaveatCondition,
				InterceptType: &lnrpc.RPCMiddlewareRequest_Request{Request: good}},
			{MsgId: 2, CustomCaveatCondition: CaveatCondition,
				InterceptType: &lnrpc.RPCMiddlewareRequest_Request{Request: open(t, bad)}},
			{MsgId: 3, CustomCaveatCondition: CaveatCondition,
				InterceptType: &lnrpc.RPCMiddlewareRequest_StreamAuth{
					StreamAuth: &lnrpc.StreamAuth{MethodFullUri: "/lnrpc.Lightning/SubscribeInvoices"}}},
			{MsgId: 4, CustomCaveatCondition: CaveatCondition,
				InterceptType: &lnrpc.RPCMiddlewareRequest_Response{
					Response: &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/GetInfo"}}},
		},
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "lnrpc.Lightning",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "RegisterRPCMiddleware",
			Handler: f.handle, ServerStreams: true, ClientStreams: true}},
	}, nil)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = Serve(ctx, conn, policy(), nil) }()
	select {
	case <-f.done:
	case <-ctx.Done():
		t.Fatal("timed out")
	}
	if f.reg.GetMiddlewareName() != Name || f.reg.GetCustomMacaroonCaveatName() != CaveatName ||
		f.reg.GetReadOnlyMode() {
		t.Fatalf("registration %+v", f.reg)
	}
	if e := f.feedback[1].GetError(); e != "" {
		t.Errorf("a good open refused: %s", e)
	}
	if f.feedback[2].GetError() == "" {
		t.Error("a push passed")
	}
	if f.feedback[3].GetError() == "" {
		t.Error("a stream passed")
	}
	if f.feedback[4].GetError() != "" || f.feedback[4].GetReplaceResponse() {
		t.Error("a response was changed")
	}
}
