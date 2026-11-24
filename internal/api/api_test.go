package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const testKey = "tfk_0123456789abcdefghijklmnopqrstuv"

type recorder struct {
	mu       sync.Mutex
	server   int
	upgrade  bool
	recorded int
}

func (r *recorder) SetProtocol(server int, upgradeRequired bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.server, r.upgrade = server, upgradeRequired
	r.recorded++
	return nil
}

// rpcServer answers each call in a request (single or batch) with answer(method, params).
func rpcServer(t *testing.T, answer func(method string, params json.RawMessage) (any, *protocol.RpcError)) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.URL.Path != protocol.RPCPath || r.Method != http.MethodPost {
			http.Error(w, "not here", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set(protocol.ProtocolHeader, fmt.Sprint(protocol.ProtocolVersion))
		w.Header().Set("Content-Type", "application/json")
		one := func(req protocol.Request) protocol.Response {
			res, rerr := answer(req.Method, req.Params)
			out := protocol.Response{JSONRPC: "2.0", ID: req.ID, Error: rerr}
			if rerr == nil {
				out.Result, _ = json.Marshal(res)
			}
			return out
		}
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			var reqs []protocol.Request
			if err := json.Unmarshal(body, &reqs); err != nil {
				t.Errorf("bad batch: %v", err)
			}
			outs := make([]protocol.Response, 0, len(reqs))
			for _, q := range reqs {
				outs = append(outs, one(q))
			}
			_ = json.NewEncoder(w).Encode(outs)
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(one(req))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestCallSendsHeadersAndDecodes(t *testing.T) {
	var gotClient, gotAuth, gotJSONRPC string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClient, gotAuth = r.Header.Get(protocol.ClientHeader), r.Header.Get("Authorization")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotJSONRPC, _ = req["jsonrpc"].(string)
		w.Header().Set(protocol.ProtocolHeader, "7")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"id":"mb_1","name":"Ann","extra_new_field":1}}`, mustJSON(req["id"]))
	}))
	defer srv.Close()
	rec := &recorder{}
	c := New(srv.URL, testKey, 5*time.Second).WithRecorder(rec)
	var out struct{ ID, Name string }
	if err := c.Call(context.Background(), "member.me", protocol.NewEmpty(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "mb_1" || out.Name != "Ann" {
		t.Fatalf("decoded %+v", out)
	}
	wantClient := fmt.Sprintf("teamflow-go/%s (protocol %d)", Version, protocol.ProtocolVersion)
	if gotClient != wantClient {
		t.Fatalf("%s = %q, want %q", protocol.ClientHeader, gotClient, wantClient)
	}
	if gotAuth != "Bearer "+testKey || gotJSONRPC != "2.0" {
		t.Fatalf("auth %q jsonrpc %q", gotAuth, gotJSONRPC)
	}
	if rec.server != 7 || rec.upgrade {
		t.Fatalf("recorded protocol %d upgrade %v", rec.server, rec.upgrade)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestNoKeyNoAuthorization(t *testing.T) {
	var auth []string
	srv, _ := rpcServer(t, func(string, json.RawMessage) (any, *protocol.RpcError) { return map[string]any{}, nil })
	srv.Config.Handler = wrapAuth(srv.Config.Handler, &auth)
	c := New(srv.URL, "", 5*time.Second)
	if err := c.Call(context.Background(), "auth.login", map[string]string{"email": "a@example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(auth) != 1 || auth[0] != "" {
		t.Fatalf("Authorization sent without a key: %q", auth)
	}
}

func wrapAuth(h http.Handler, seen *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		h.ServeHTTP(w, r)
	})
}

func TestBatchSplitsAtMaxBatch(t *testing.T) {
	srv, requests := rpcServer(t, func(_ string, params json.RawMessage) (any, *protocol.RpcError) {
		var p struct{ N int }
		_ = json.Unmarshal(params, &p)
		if p.N == 77 {
			return nil, &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "no", Data: map[string]any{"reason": "NOT_A_MEMBER"}}
		}
		return map[string]int{"n": p.N}, nil
	})
	c := New(srv.URL, testKey, 5*time.Second)
	calls := make([]Call, 120)
	for i := range calls {
		calls[i] = Call{Method: "signal.emit", Params: map[string]int{"n": i}}
	}
	res, err := c.Batch(context.Background(), calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Fatalf("%d HTTP requests for 120 calls, want 3 (MaxBatch %d)", got, protocol.MaxBatch)
	}
	if len(res) != 120 {
		t.Fatalf("%d results", len(res))
	}
	for i, r := range res {
		if i == 77 {
			var ae *APIError
			if !errors.As(r.Err, &ae) || ae.Reason != "NOT_A_MEMBER" {
				t.Fatalf("result 77: %v", r.Err)
			}
			continue
		}
		var out struct{ N int }
		if err := r.Decode(&out); err != nil || out.N != i {
			t.Fatalf("result %d = %+v (%v): answers out of order", i, out, err)
		}
	}
}

func TestBatchEmpty(t *testing.T) {
	c := New("https://teamflow.example.com", testKey, time.Second)
	c.http.Transport = refuseTransport{t}
	res, err := c.Batch(context.Background(), nil)
	if err != nil || len(res) != 0 {
		t.Fatalf("empty batch: %v %v", res, err)
	}
}

func TestBatchMissingAnswerIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"jsonrpc":"2.0","id":0,"result":{}}]`) // answers only the first of two
	}))
	defer srv.Close()
	res, err := New(srv.URL, testKey, time.Second).Batch(context.Background(),
		[]Call{{Method: "signal.emit"}, {Method: "signal.emit"}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil || res[1].Err == nil {
		t.Fatalf("results %+v", res)
	}
}

type refuseTransport struct{ t *testing.T }

func (r refuseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("a request was sent")
	return nil, errors.New("refused")
}

func TestHTTPToNonLoopbackRefusedBeforeSending(t *testing.T) {
	for _, server := range []string{"http://teamflow.example.com", "http://10.0.0.5:8700", "http://127.0.0.2",
		"ftp://teamflow.example.com", "teamflow.example.com", "http://localhost.example.com", "://bad"} {
		c := New(server, testKey, time.Second)
		c.http.Transport = refuseTransport{t}
		err := c.Call(context.Background(), "member.me", protocol.NewEmpty(), nil)
		if !errors.Is(err, ErrInsecure) {
			t.Fatalf("%s: want ErrInsecure, got %v", server, err)
		}
		if _, err := c.Batch(context.Background(), []Call{{Method: "member.me"}}); !errors.Is(err, ErrInsecure) {
			t.Fatalf("%s: Batch: want ErrInsecure, got %v", server, err)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatal("key in the error")
		}
	}
}

func TestLoopbackHTTPAndHTTPSAllowed(t *testing.T) {
	for _, server := range []string{"http://localhost:8700", "http://127.0.0.1:8700/", "http://[::1]:8700",
		"https://teamflow.example.com", "HTTPS://teamflow.example.com/base/"} {
		if err := New(server, testKey, time.Second).err; err != nil {
			t.Fatalf("%s refused: %v", server, err)
		}
	}
	if got := New("https://teamflow.example.com/base/", "", time.Second).url; got != "https://teamflow.example.com/base"+protocol.RPCPath {
		t.Fatalf("url %q", got)
	}
}

func TestUpgradeRequiredBecomesErrUpgradeRequired(t *testing.T) {
	srv, _ := rpcServer(t, func(string, json.RawMessage) (any, *protocol.RpcError) {
		return nil, &protocol.RpcError{Code: protocol.CodeRuleFailed,
			Message: "update teamflow: this server needs client protocol ≥ 2",
			Data:    map[string]any{"reason": "UPGRADE_REQUIRED"}}
	})
	rec := &recorder{}
	c := New(srv.URL, testKey, time.Second).WithRecorder(rec)
	err := c.Call(context.Background(), "member.me", protocol.NewEmpty(), nil)
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("want ErrUpgradeRequired, got %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Reason != "UPGRADE_REQUIRED" || ae.Code != protocol.CodeRuleFailed {
		t.Fatalf("APIError %+v", ae)
	}
	if !strings.HasPrefix(err.Error(), "update teamflow") {
		t.Fatalf("message %q", err)
	}
	if !rec.upgrade {
		t.Fatal("UPGRADE_REQUIRED was not recorded")
	}
	// in a batch too
	res, err := c.Batch(context.Background(), []Call{{Method: "signal.emit"}})
	if err != nil || !errors.Is(res[0].Err, ErrUpgradeRequired) {
		t.Fatalf("batch: %v %v", res, err)
	}
	// other rule failures are not upgrades
	other := &APIError{Code: protocol.CodeRuleFailed, Reason: "NOT_A_MEMBER", Message: "no"}
	if errors.Is(other, ErrUpgradeRequired) {
		t.Fatal("NOT_A_MEMBER is not UPGRADE_REQUIRED")
	}
}

func TestKeyNeverInErrorText(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"500 echo": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprintf(w, "boom: %s", r.Header.Get("Authorization"))
		},
		"rpc error echo": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"bad key %s","data":{"reason":"%s"}}}`,
				testKey, testKey)
		},
		"400 echo": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			fmt.Fprintf(w, "not json %s", testKey)
		},
		"garbage 200": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"jsonrpc": %s`, testKey)
		},
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		c := New(srv.URL, testKey, time.Second)
		err := c.Call(context.Background(), "member.me", protocol.NewEmpty(), nil)
		_, berr := c.Batch(context.Background(), []Call{{Method: "member.me"}})
		srv.Close()
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprint(berr)} {
			if strings.Contains(text, testKey) || strings.Contains(text, testKey[4:]) {
				t.Fatalf("%s: key in the error text: %s", name, text)
			}
		}
	}
}

func TestClientStringMasksKey(t *testing.T) {
	c := New("https://teamflow.example.com", testKey, time.Second)
	for _, text := range []string{c.String(), fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c), fmt.Sprintf("%+v", *c)} {
		if strings.Contains(text, testKey[4:]) {
			t.Fatalf("key shown: %s", text)
		}
	}
}

func TestOfflineIsErrOffline(t *testing.T) {
	// nothing listening
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(url, testKey, time.Second)
	if err := c.Call(context.Background(), "member.me", protocol.NewEmpty(), nil); !errors.Is(err, ErrOffline) {
		t.Fatalf("closed port: want ErrOffline, got %v", err)
	}
	if _, err := c.Batch(context.Background(), []Call{{Method: "member.me"}}); !errors.Is(err, ErrOffline) {
		t.Fatalf("batch on a closed port: want ErrOffline, got %v", err)
	}
	// a 5xx
	five := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer five.Close()
	if err := New(five.URL, testKey, time.Second).Call(context.Background(), "member.me", nil, nil); !errors.Is(err, ErrOffline) {
		t.Fatalf("503: want ErrOffline, got %v", err)
	}
	// too slow: the timeout
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	start := time.Now()
	err := New(slow.URL, testKey, 200*time.Millisecond).Call(context.Background(), "member.me", nil, nil)
	if !errors.Is(err, ErrOffline) || time.Since(start) > time.Second {
		t.Fatalf("timeout: want ErrOffline within the timeout, got %v after %v", err, time.Since(start))
	}
	// a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(slow.URL, testKey, time.Second).Call(ctx, "member.me", nil, nil); !errors.Is(err, ErrOffline) {
		t.Fatalf("cancelled: want ErrOffline, got %v", err)
	}
}

func TestNonJSONAnswerIsAnAPIErrorNotOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(413)
		fmt.Fprint(w, "too large")
	}))
	defer srv.Close()
	err := New(srv.URL, testKey, time.Second).Call(context.Background(), "member.me", nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || errors.Is(err, ErrOffline) || ae.Code != 413 {
		t.Fatalf("413: %v", err)
	}
}

func TestOversizedRequestRefusedBeforeSending(t *testing.T) {
	c := New("https://teamflow.example.com", testKey, time.Second)
	c.http.Transport = refuseTransport{t}
	big := strings.Repeat("x", protocol.MaxBodyBytes)
	err := c.Call(context.Background(), "artifact.write", map[string]string{"content": big}, nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestUnmarshalableParamsAreAnError(t *testing.T) {
	c := New("https://teamflow.example.com", testKey, time.Second)
	c.http.Transport = refuseTransport{t}
	if err := c.Call(context.Background(), "member.me", map[string]any{"f": func() {}}, nil); err == nil {
		t.Fatal("bad params sent")
	}
}

func TestBadResultShapeIsAnError(t *testing.T) {
	srv, _ := rpcServer(t, func(string, json.RawMessage) (any, *protocol.RpcError) { return "a string", nil })
	var out struct{ ID string }
	if err := New(srv.URL, testKey, time.Second).Call(context.Background(), "member.me", nil, &out); err == nil {
		t.Fatal("a string decoded into a struct")
	}
}

func TestNilParamsSendEmptyObject(t *testing.T) {
	var got json.RawMessage
	srv, _ := rpcServer(t, func(_ string, p json.RawMessage) (any, *protocol.RpcError) { got = p; return map[string]any{}, nil })
	if err := New(srv.URL, testKey, time.Second).Call(context.Background(), "member.me", nil, nil); err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Fatalf("params %s, want {}", got)
	}
}

func TestProtocolRecordedInTheStore(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	refuse := false
	srv, _ := rpcServer(t, func(string, json.RawMessage) (any, *protocol.RpcError) {
		if refuse {
			return nil, &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "too old",
				Data: map[string]any{"reason": "UPGRADE_REQUIRED"}}
		}
		return map[string]any{}, nil
	})
	c := New(srv.URL, testKey, time.Second).WithRecorder(st)
	if err := c.Call(context.Background(), "member.me", nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ProtocolOK(); !ok || st.ServerProtocol() != protocol.ProtocolVersion {
		t.Fatalf("after a served call: ok=%v server=%d", ok, st.ServerProtocol())
	}
	refuse = true
	_ = c.Call(context.Background(), "member.me", nil, nil)
	if ok, msg := st.ProtocolOK(); ok || !strings.Contains(msg, "update teamflow") {
		t.Fatalf("after UPGRADE_REQUIRED: ok=%v %q", ok, msg)
	}
	refuse = false
	_ = c.Call(context.Background(), "member.me", nil, nil)
	if ok, _ := st.ProtocolOK(); !ok {
		t.Fatal("served again but the store still says upgrade")
	}
}

func TestJSONThatIsNotJSONRPCIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"detail":"no"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, testKey, time.Second)
	var out map[string]any
	if err := c.Call(context.Background(), "member.me", nil, &out); err == nil {
		t.Fatal("a non-JSON-RPC answer was taken as a result")
	}
	if _, err := c.Batch(context.Background(), []Call{{Method: "member.me"}}); err == nil {
		t.Fatal("a non-JSON-RPC answer was taken as a batch result")
	}
}
