package knots

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// node answers as Knots would, by method
func node(t *testing.T, answers map[string]string) *Client {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "donations" || pass != "secret" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answer, ok := answers[req.Method]
		if !ok {
			t.Errorf("unexpected %s", req.Method)
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(s.Close)
	return New(s.URL, "donations", "secret")
}

func TestSats(t *testing.T) {
	for in, want := range map[string]int64{"0.1": 10_000_000, "0.00000001": 1,
		"21000000": 2_100_000_000_000_000, "0": 0} {
		got, err := Sats(json.Number(in))
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"0.000000001", "-1", "x"} {
		if _, err := Sats(json.Number(in)); err == nil {
			t.Fatalf("%s was read", in)
		}
	}
}

func TestCalls(t *testing.T) {
	ctx := context.Background()
	c := node(t, map[string]string{
		"gettxout":           `{"result":{"value":0.00012345,"confirmations":6,"scriptPubKey":{"hex":"5120aa","address":"bc1pxyz"}},"error":null}`,
		"testmempoolaccept":  `{"result":[{"txid":"ab","allowed":false,"reject-reason":"bad-txns-inputs-missingorspent"}],"error":null}`,
		"sendrawtransaction": `{"result":null,"error":{"code":-27,"message":"Transaction already in block chain"}}`,
		"estimatesmartfee":   `{"result":{"feerate":0.00002,"blocks":3},"error":null}`,
		"getblockcount":      `{"result":977400,"error":null}`,
	})
	out, err := c.TxOut(ctx, "ab", 0, true)
	if err != nil || out.ValueSat != 12345 || out.Confirmations != 6 || out.Address != "bc1pxyz" {
		t.Fatalf("%+v %v", out, err)
	}
	acc, err := c.TestAccept(ctx, "00")
	if err != nil || acc.Allowed || acc.Reason != "bad-txns-inputs-missingorspent" {
		t.Fatalf("%+v %v", acc, err)
	}
	if _, err := c.Send(ctx, "00"); err != nil {
		t.Fatalf("a known transaction is an error: %v", err)
	}
	rate, err := c.FeeRate(ctx, 3)
	if err != nil || rate != 2 {
		t.Fatalf("%v %v", rate, err)
	}
	h, err := c.Height(ctx)
	if err != nil || h != 977400 {
		t.Fatalf("%d %v", h, err)
	}
}

func TestSpentAndRefused(t *testing.T) {
	ctx := context.Background()
	c := node(t, map[string]string{
		"gettxout":           `{"result":null,"error":null}`,
		"sendrawtransaction": `{"result":null,"error":{"code":-26,"message":"mandatory-script-verify-flag-failed"}}`,
		"estimatesmartfee":   `{"result":{"errors":["Insufficient data"],"blocks":0},"error":null}`,
	})
	out, err := c.TxOut(ctx, "ab", 0, true)
	if err != nil || out != nil {
		t.Fatalf("spent: %+v %v", out, err)
	}
	var rpc *Error
	if _, err := c.Send(ctx, "00"); !errors.As(err, &rpc) || rpc.Code != -26 {
		t.Fatalf("refused: %v", err)
	}
	if rate, err := c.FeeRate(ctx, 3); err != nil || rate != 0 {
		t.Fatalf("no estimate: %v %v", rate, err)
	}
	bad := New(c.URL, "donations", "wrong")
	if _, err := bad.Height(ctx); err == nil {
		t.Fatal("a refused login was not an error")
	}
}
