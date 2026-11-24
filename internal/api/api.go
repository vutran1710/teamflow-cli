// Package api is the JSON-RPC client for the Teamflow server (POST <server>/rpc).
//
// Every request carries Teamflow-Client: teamflow-go/<Version> (protocol N) and, when there is a key, the
// Authorization header: the key goes nowhere else. It is never part of an error string, and a Client prints it
// masked. A plain-http server is refused before anything is sent, except on loopback.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Version is the client's version, set at build time (-ldflags "-X .../internal/api.Version=1.2.3").
var Version = "dev"

// UpdateMessage is what the person is told when the server needs a newer client.
const UpdateMessage = "update teamflow (`teamflow self-update`, or download the new release)"

var (
	// ErrOffline means the server could not be reached, or failed (5xx, timeout): try again later.
	ErrOffline = errors.New("teamflow server unreachable")
	// ErrUpgradeRequired matches an APIError with reason UPGRADE_REQUIRED: the person must update teamflow.
	ErrUpgradeRequired = errors.New("UPGRADE_REQUIRED")
	// ErrInsecure means the server URL is not https (plain http is allowed on loopback only).
	ErrInsecure = errors.New("teamflow server must be https (plain http only on localhost)")
	// ErrServerFault marks an HTTP 500: the server failed on what it was sent (it is also ErrOffline: try again).
	// 502, 503 and 504 are an outage, not a fault of the request.
	ErrServerFault = errors.New("server fault")
	// ErrTooLarge means a request is over the server's body limit (MAX_BODY_BYTES).
	ErrTooLarge = errors.New("request too large")
)

// APIError is the server's JSON-RPC error answer (or an answer that is not JSON-RPC: Code is then the HTTP status).
type APIError struct {
	Code    int
	Reason  string // the reason in the error's data ("" when none)
	Message string
}

func (e *APIError) Error() string {
	if e.Reason == string(protocol.RejectReasonUpgradeRequired) {
		return fmt.Sprintf("%s (%s)", UpdateMessage, e.Message)
	}
	if e.Reason != "" {
		return e.Reason + ": " + e.Message
	}
	return strconv.Itoa(e.Code) + ": " + e.Message
}

// Is makes errors.Is(err, ErrUpgradeRequired) true for UPGRADE_REQUIRED answers.
func (e *APIError) Is(target error) bool {
	return target == ErrUpgradeRequired && e.Reason == string(protocol.RejectReasonUpgradeRequired)
}

// ProtocolRecorder keeps what the server said about protocols (store.Store is one).
type ProtocolRecorder interface {
	SetProtocol(server int, upgradeRequired bool) error
}

// Client calls one server with one key (or none, to sign in).
type Client struct {
	url  string
	key  string
	err  error // a refused server URL: every call returns it, before sending
	http *http.Client
	rec  ProtocolRecorder
	gate Gate
}

// Gate decides whether and when one call is sent: it runs send (the call itself) or refuses with its own error.
// The outbox uses it to send nothing ahead of queued signals (see core).
type Gate func(ctx context.Context, method string, send func() error) error

// New makes a client for server with key ("" for none); each request gives up after timeout.
func New(server, key string, timeout time.Duration) *Client {
	c := &Client{key: key, http: &http.Client{
		Timeout: timeout,
		// Never follow a redirect: Go would replay the POST with its Authorization header, even to plain http. A 3xx
		// is answered as an error instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	c.url, c.err = rpcURL(server)
	return c
}

// WithRecorder records the server's Teamflow-Protocol header and UPGRADE_REQUIRED answers into r.
func (c *Client) WithRecorder(r ProtocolRecorder) *Client {
	c.rec = r
	return c
}

// WithGate passes every single call (Call, never Batch) through g.
func (c *Client) WithGate(g Gate) *Client {
	c.gate = g
	return c
}

// Ungated is the same client without its gate: the outbox's own sender.
func (c *Client) Ungated() *Client {
	u := *c
	u.gate = nil
	return &u
}

// String shows the client with its key masked.
func (c *Client) String() string {
	return fmt.Sprintf("api.Client{server: %s, key: %s}", c.url, mask(c.key))
}

// GoString is String, so %#v masks the key too.
func (c *Client) GoString() string { return c.String() }

// Format masks the key under every verb (a Client value, not only a pointer, may be printed).
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func mask(key string) string {
	if key == "" {
		return "none"
	}
	prefix, _, ok := strings.Cut(key, "_")
	if !ok || len(prefix) > 4 {
		return "***"
	}
	return prefix + "_***"
}

func rpcURL(server string) (string, error) {
	u, err := serverURL(server)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + protocol.RPCPath
	u.RawPath = ""
	return u.String(), nil
}

// serverURL is the server's URL checked (https, or http on loopback), with no userinfo, query or fragment.
func serverURL(server string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: the server is not a URL like https://teamflow.example.com", ErrInsecure)
	}
	u.User = nil // a key pasted as userinfo is never sent nor shown
	u.RawQuery, u.Fragment = "", ""
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, fmt.Errorf("%w: refusing %s", ErrInsecure, u.String())
		}
	default:
		return nil, fmt.Errorf("%w: refusing %s", ErrInsecure, u.String())
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return u, nil
}

// LiveURL is the server's live socket for a project: wss://<server>/live?org=&project= (ws:// only on loopback,
// as plain http is). Ids only: a key or a ticket never goes in it.
func LiveURL(server, org, project string) (string, error) {
	u, err := serverURL(server)
	if err != nil {
		return "", err
	}
	u.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	u.Path = strings.TrimRight(u.Path, "/") + protocol.LivePath
	u.RawPath = ""
	u.RawQuery = url.Values{"org": {org}, "project": {project}}.Encode()
	return u.String(), nil
}

// ClientName is the Teamflow-Client header's value: this build and its protocol.
func ClientName() string {
	return fmt.Sprintf("teamflow-go/%s (protocol %d)", Version, protocol.ProtocolVersion)
}

func loopback(host string) bool {
	switch strings.ToLower(host) {
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// Call is one call of a batch: Params is marshalled as is (build it with a protocol.New<Type>() constructor).
type Call struct {
	Method string
	Params any
}

// Result is the answer to one call of a batch: the raw result, or the call's error.
type Result struct {
	Result json.RawMessage
	Err    error
}

// Decode reads the result into out, or returns the call's error.
func (r Result) Decode(out any) error {
	if r.Err != nil {
		return r.Err
	}
	return decode(r.Result, out)
}

func decode(raw json.RawMessage, out any) error {
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected result shape: %w", err)
	}
	return nil
}

// request is one call's JSON.
func request(id int, method string, params any) ([]byte, error) {
	var raw json.RawMessage = json.RawMessage("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("%s params: %w", method, err)
		}
		raw = b
	}
	return json.Marshal(protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: raw})
}

func tooLarge(n int) error {
	return fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, n, protocol.MaxBodyBytes)
}

// Call makes one call and decodes its result into out (nil: ignore the result), through the client's gate when it
// has one.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if c.err != nil {
		return c.err
	}
	if c.gate != nil {
		return c.gate(ctx, method, func() error { return c.call(ctx, method, params, out) })
	}
	return c.call(ctx, method, params, out)
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	req, err := request(1, method, params)
	if err != nil {
		return err
	}
	if len(req) > protocol.MaxBodyBytes {
		return tooLarge(len(req))
	}
	body, server, err := c.post(ctx, req)
	if err != nil {
		return err
	}
	if body == nil { // 204: nothing to read
		c.record(server, false)
		return decode(nil, out)
	}
	var resp protocol.Response
	if err := json.Unmarshal(body, &resp); err != nil || (resp.Error == nil && resp.Result == nil) {
		return &APIError{Code: protocol.CodeParseError, Message: "the server's answer is not JSON-RPC"}
	}
	if resp.Error != nil {
		ae := c.rpcError(resp.Error)
		c.record(server, isUpgrade(ae))
		return ae
	}
	c.record(server, false)
	return decode(resp.Result, out)
}

// Batch sends calls as JSON-RPC batches of at most MaxBatch calls and MaxBodyBytes; one Result per call, in
// order. A call too large on its own gets ErrTooLarge and is not sent. On a failure that ends the batch (offline,
// a refused answer) it returns the results of the calls before the failed request, and the error.
func (c *Client) Batch(ctx context.Context, calls []Call) ([]Result, error) {
	out := make([]Result, 0, len(calls))
	err := c.BatchEach(ctx, calls, func(_, _ int, res []Result, _ error) bool {
		out = append(out, res...)
		return false
	})
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, err
}

// BatchEach sends calls as Batch does, one request at a time, and hands each request's answers to fn before the
// next request goes out: calls[start:end] and their results (a call too large on its own is among them, with
// ErrTooLarge, unsent). fn returning true stops it there; nothing after end is sent. A failure that ends a request
// (offline, a refused answer) is handed to fn with the request's span and no results, and returned.
func (c *Client) BatchEach(ctx context.Context, calls []Call, fn func(start, end int, res []Result, err error) (stop bool)) error {
	if c.err != nil {
		return c.err
	}
	// Measure each call with the widest id a chunk uses, so a chunk packed by these sizes fits.
	wideID := protocol.MaxBatch - 1
	sizes := make([]int, len(calls))
	for i, call := range calls {
		b, err := request(wideID, call.Method, call.Params)
		if err != nil {
			return err
		}
		sizes[i] = len(b)
	}
	out := make([]Result, len(calls))
	for start := 0; start < len(calls); {
		// pack calls[start:end] (skipping those too large alone) under both limits
		var chunk []int
		size := 2 // [ ]
		end := start
		for ; end < len(calls) && len(chunk) < protocol.MaxBatch; end++ {
			if sizes[end]+2 > protocol.MaxBodyBytes {
				out[end] = Result{Err: tooLarge(sizes[end] + 2)}
				continue
			}
			add := sizes[end]
			if len(chunk) > 0 {
				add++ // ,
			}
			if size+add > protocol.MaxBodyBytes {
				break
			}
			size += add
			chunk = append(chunk, end)
		}
		if len(chunk) > 0 {
			if err := c.batchChunk(ctx, calls, chunk, out); err != nil {
				fn(start, end, nil, err)
				return err
			}
		}
		if fn(start, end, out[start:end], nil) {
			return nil
		}
		start = end
	}
	return nil
}

// batchChunk sends the calls at idx in one request and puts their results into out.
func (c *Client) batchChunk(ctx context.Context, calls []Call, idx []int, out []Result) error {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, at := range idx {
		b, err := request(i, calls[at].Method, calls[at].Params)
		if err != nil {
			return err
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(b)
	}
	buf.WriteByte(']')
	body, server, err := c.post(ctx, buf.Bytes())
	if err != nil {
		return err
	}
	var resps []protocol.Response
	if err := json.Unmarshal(body, &resps); err != nil {
		var one protocol.Response // the server could not read the batch at all
		if json.Unmarshal(body, &one) == nil && one.Error != nil {
			ae := c.rpcError(one.Error)
			c.record(server, isUpgrade(ae))
			return ae
		}
		return &APIError{Code: protocol.CodeParseError, Message: "the server's answer is not JSON-RPC"}
	}
	upgrade := false
	byID := make(map[string]protocol.Response, len(resps))
	for _, r := range resps {
		byID[string(bytes.TrimSpace(r.ID))] = r
	}
	for i, at := range idx {
		r, ok := byID[strconv.Itoa(i)]
		switch {
		case !ok:
			out[at] = Result{Err: &APIError{Code: protocol.CodeInternalError, Message: "no answer for this call"}}
		case r.Error == nil && r.Result == nil:
			out[at] = Result{Err: &APIError{Code: protocol.CodeParseError, Message: "the server's answer is not JSON-RPC"}}
		case r.Error != nil:
			ae := c.rpcError(r.Error)
			upgrade = upgrade || isUpgrade(ae)
			out[at] = Result{Err: ae}
		default:
			out[at] = Result{Result: r.Result}
		}
	}
	c.record(server, upgrade)
	return nil
}

// post sends one body and returns the answer's body (nil for 204) and the server's protocol (0: not said).
func (c *Client) post(ctx context.Context, body []byte) ([]byte, int, error) {
	if len(body) > protocol.MaxBodyBytes {
		return nil, 0, tooLarge(len(body))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrOffline, c.redactText(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.ClientHeader, ClientName())
	if c.key != "" { // signing in carries no key
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrOffline, c.redactText(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// The server never redirects: a 3xx is a portal, a proxy or a forger. Its body and headers are not read.
		return nil, 0, &APIError{Code: resp.StatusCode, Message: "unexpected redirect"}
	}
	server, _ := strconv.Atoi(strings.TrimSpace(resp.Header.Get(protocol.ProtocolHeader)))
	if resp.StatusCode == http.StatusTooManyRequests { // busy: transient, like a 5xx, whatever the body says
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, 0, fmt.Errorf("%w: server busy (429), try again later", ErrOffline)
	}
	if resp.StatusCode == http.StatusInternalServerError {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, 0, fmt.Errorf("%w: %w: server error 500", ErrOffline, ErrServerFault)
	}
	if resp.StatusCode >= 500 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, 0, fmt.Errorf("%w: server error %d", ErrOffline, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, server, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxBodyBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrOffline, c.redactText(err.Error()))
	}
	if len(data) > protocol.MaxBodyBytes {
		return nil, 0, &APIError{Code: resp.StatusCode, Message: "the server's answer is too large"}
	}
	if resp.StatusCode >= 300 && !json.Valid(data) {
		return nil, 0, &APIError{Code: resp.StatusCode, Message: fmt.Sprintf("unexpected answer (HTTP %d)", resp.StatusCode)}
	}
	return data, server, nil
}

// record keeps what an answer said about protocols; best effort: a busy store must not fail the call.
func (c *Client) record(server int, upgradeRequired bool) {
	if c.rec != nil && (server > 0 || upgradeRequired) {
		_ = c.rec.SetProtocol(server, upgradeRequired)
	}
}

func (c *Client) rpcError(e *protocol.RpcError) *APIError {
	return c.redact(&APIError{Code: int(e.Code), Reason: string(e.RejectReason()), Message: e.Message})
}

func isUpgrade(err error) bool { return errors.Is(err, ErrUpgradeRequired) }

func (c *Client) redact(e *APIError) *APIError {
	e.Reason, e.Message = c.redactText(e.Reason), c.redactText(e.Message)
	return e
}

// redactText takes the key out of text the server or the transport produced.
func (c *Client) redactText(s string) string {
	if c.key == "" {
		return s
	}
	s = strings.ReplaceAll(s, c.key, mask(c.key))
	if _, secret, ok := strings.Cut(c.key, "_"); ok && len(secret) >= 8 {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

// KeyRef names a key without revealing it: the first 16 hex digits of its SHA-256 (as the Python client's key_ref).
// Spool rows carry it, so each waits for its own key. "" for no key.
func KeyRef(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// KeyRef is the client's key's ref (see KeyRef).
func (c *Client) KeyRef() string { return KeyRef(c.key) }
