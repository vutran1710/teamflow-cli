package protocol

import "encoding/json"

// JSONRPCVersion is the only JSON-RPC version the server speaks.
const JSONRPCVersion = "2.0"

// Request is one JSON-RPC call. A call without an ID is a notification and gets no answer.
type Request struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one JSON-RPC answer: a Result or an Error. ID is null when the server could not read the call.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RpcError       `json:"error,omitempty"`
}

// RejectReason is the reason in the error's data, or "" when the server gave none.
func (e RpcError) RejectReason() RejectReason {
	r, _ := e.Data["reason"].(string)
	return RejectReason(r)
}
