package guard

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/paulscode/lightning-fork-swap/donations/internal/guard/lnrpc"
)

// Name is how the guard registers itself with lnd.
const Name = "lfswap-donations-guard"

var streamDesc = &grpc.StreamDesc{
	StreamName: "RegisterRPCMiddleware", ServerStreams: true, ClientStreams: true,
}

const method = "/lnrpc.Lightning/RegisterRPCMiddleware"

// Dial opens lnd's gRPC port, trusting only its certificate, and sends the
// macaroon (one that may only register middleware) with every call.
func Dial(target, certPath, macaroonPath string) (*grpc.ClientConn, error) {
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		return nil, fmt.Errorf("no certificate in %s", certPath)
	}
	mac, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(target,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
		grpc.WithPerRPCCredentials(macaroonCreds(hex.EncodeToString(mac))))
}

type macaroonCreds string

func (m macaroonCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": string(m)}, nil
}
func (macaroonCreds) RequireTransportSecurity() bool { return true }

// Serve registers with lnd and answers its interceptions until the stream
// ends or the context does. Every request is judged by the policy; every
// response is passed on unchanged.
func Serve(ctx context.Context, conn grpc.ClientConnInterface, p Policy, logger *log.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := conn.NewStream(ctx, streamDesc, method)
	if err != nil {
		return err
	}
	if err := stream.SendMsg(&lnrpc.RPCMiddlewareResponse{
		MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Register{
			Register: &lnrpc.MiddlewareRegistration{
				MiddlewareName: Name, CustomMacaroonCaveatName: CaveatName,
			},
		},
	}); err != nil {
		return err
	}
	for {
		var req lnrpc.RPCMiddlewareRequest
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		feedback := &lnrpc.InterceptFeedback{}
		switch t := req.InterceptType.(type) {
		case *lnrpc.RPCMiddlewareRequest_RegComplete:
			if logger != nil {
				logger.Printf("registered with lnd for the %q caveat", CaveatName)
			}
			continue
		case *lnrpc.RPCMiddlewareRequest_StreamAuth:
			feedback.Error = refuse("no streaming calls").Error()
		case *lnrpc.RPCMiddlewareRequest_Request:
			if err := p.Check(ctx, req.CustomCaveatCondition, t.Request); err != nil {
				feedback.Error = err.Error()
				if logger != nil {
					logger.Printf("refused %s: %v", t.Request.GetMethodFullUri(), err)
				}
			}
		case *lnrpc.RPCMiddlewareRequest_Response:
			// Passed on as it is
		default:
			feedback.Error = refuse("unknown interception").Error()
		}
		if err := stream.SendMsg(&lnrpc.RPCMiddlewareResponse{
			RefMsgId:          req.MsgId,
			MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Feedback{Feedback: feedback},
		}); err != nil {
			return err
		}
	}
}

// Run keeps the guard registered: after any error it registers again,
// waiting longer each time (up to a minute). While it is not registered,
// lnd refuses every request made with the worker's macaroon.
func Run(ctx context.Context, conn grpc.ClientConnInterface, p Policy, logger *log.Logger) {
	wait := time.Second
	for {
		start := time.Now()
		err := Serve(ctx, conn, p, logger)
		if ctx.Err() != nil {
			return
		}
		if logger != nil {
			logger.Printf("stream ended: %v", err)
		}
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < time.Minute {
			wait *= 2
		}
	}
}
