// Package api is the JSON-RPC client for the Teamflow server (POST <server>/rpc).
//
// Every request carries Teamflow-Client: teamflow-go/<Version> (protocol N) and, when there is a key, the
// Authorization header: the key goes nowhere else. It is never part of an error string, and a Client prints it
// masked. A plain-http server is refused before anything is sent, except on loopback.
package api

import (
	"bytes"
	"context"
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
const UpdateMessage = "update teamflow: install the new release (this server needs a newer client)"

var (
	// ErrOffline means the server could not be reached, or failed (5xx, timeout): try again later.
	ErrOffline = errors.New("teamflow server unreachable")
	// ErrUpgradeRequired matches an APIError with reason UPGRADE_REQUIRED: the person must update teamflow.
	ErrUpgradeRequired = errors.New("UPGRADE_REQUIRED")
	// ErrInsecure means the server URL is not https (plain http is allowed on loopback only).
	ErrInsecure = errors.New("teamflow server must be https (plain http only on localhost)")
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
}

// New makes a client for server with key ("" for none); each request gives up after timeout.
func New(server, key string, timeout time.Duration) *Client {
	c := &Client{key: key, http: &http.Client{Timeout: timeout}}
	c.url, c.err = rpcURL(server)
	return c
}

// WithRecorder records the server's Teamflow-Protocol header and UPGRADE_REQUIRED answers into r.
func (c *Client) WithRecorder(r ProtocolRecorder) *Client {
	c.rec = r
	return c
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
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: %q is not a server URL", ErrInsecure, redactURL(server))
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return "", fmt.Errorf("%w: refusing %s", ErrInsecure, u.Redacted())
		}
	default:
		return "", fmt.Errorf("%w: refusing %s", ErrInsecure, u.Redacted())
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.Path = strings.TrimRight(u.Path, "/") + protocol.RPCPath
	u.RawPath = ""
	return u.String(), nil
}

func redactURL(s string) string {
	if u, err := url.Parse(s); err == nil {
		return u.Redacted()
	}
	return "(unreadable)"
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

func request(id int, method string, params any) (protocol.Request, error) {
	var raw json.RawMessage = json.RawMessage("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return protocol.Request{}, fmt.Errorf("%s params: %w", method, err)
		}
		raw = b
	}
	return protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: raw}, nil
}

// Call makes one call and decodes its result into out (nil: ignore the result).
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if c.err != nil {
		return c.err
	}
	req, err := request(1, method, params)
	if err != nil {
		return err
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

// Batch sends calls as JSON-RPC batches of at most MaxBatch; one Result per call, in order. On a failure that ends
// the batch (offline, a refused answer) it returns the results so far and the error.
func (c *Client) Batch(ctx context.Context, calls []Call) ([]Result, error) {
	if c.err != nil {
		return nil, c.err
	}
	out := make([]Result, 0, len(calls))
	for start := 0; start < len(calls); start += protocol.MaxBatch {
		chunk := calls[start:min(start+protocol.MaxBatch, len(calls))]
		reqs := make([]protocol.Request, len(chunk))
		for i, call := range chunk {
			r, err := request(i, call.Method, call.Params)
			if err != nil {
				return out, err
			}
			reqs[i] = r
		}
		body, server, err := c.post(ctx, reqs)
		if err != nil {
			return out, err
		}
		var resps []protocol.Response
		if err := json.Unmarshal(body, &resps); err != nil {
			var one protocol.Response // the server could not read the batch at all
			if json.Unmarshal(body, &one) == nil && one.Error != nil {
				ae := c.rpcError(one.Error)
				c.record(server, isUpgrade(ae))
				return out, ae
			}
			return out, &APIError{Code: protocol.CodeParseError, Message: "the server's answer is not JSON-RPC"}
		}
		upgrade := false
		byID := make(map[string]protocol.Response, len(resps))
		for _, r := range resps {
			byID[string(bytes.TrimSpace(r.ID))] = r
		}
		for i := range chunk {
			r, ok := byID[strconv.Itoa(i)]
			switch {
			case !ok:
				out = append(out, Result{Err: &APIError{Code: protocol.CodeInternalError, Message: "no answer for this call"}})
			case r.Error == nil && r.Result == nil:
				out = append(out, Result{Err: &APIError{Code: protocol.CodeParseError, Message: "the server's answer is not JSON-RPC"}})
			case r.Error != nil:
				ae := c.rpcError(r.Error)
				upgrade = upgrade || isUpgrade(ae)
				out = append(out, Result{Err: ae})
			default:
				out = append(out, Result{Result: r.Result})
			}
		}
		c.record(server, upgrade)
	}
	return out, nil
}

// post sends one body and returns the answer's body (nil for 204) and the server's protocol (0: not said).
func (c *Client) post(ctx context.Context, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	if len(body) > protocol.MaxBodyBytes {
		return nil, 0, fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, len(body), protocol.MaxBodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrOffline, c.redactText(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.ClientHeader, fmt.Sprintf("teamflow-go/%s (protocol %d)", Version, protocol.ProtocolVersion))
	if c.key != "" { // signing in carries no key
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrOffline, c.redactText(err.Error()))
	}
	defer resp.Body.Close()
	server, _ := strconv.Atoi(strings.TrimSpace(resp.Header.Get(protocol.ProtocolHeader)))
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
