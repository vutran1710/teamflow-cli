// Package protocol holds the Teamflow wire types: JSON-RPC envelopes, params and results, sync events and live
// messages. The types and constants in *_gen.go are generated from the protocol exported by the server
// (protocol/schema.json and protocol/methods.json); never edit them by hand, run `go run ./tools/gen`.
//
// Unknown JSON fields are ignored everywhere, and a union variant this client does not know (an event type from
// a newer server, say) decodes to its Unknown type instead of failing.
package protocol

//go:generate go run ../../tools/gen -protocol ../../protocol -out .

// ClientName is this client's name in the Teamflow-Client header.
const ClientName = "teamflow-go"
