# Development

## Repository layout

- `cmd/teamflow`: the binary.
- `internal/cli`: the commands. `internal/core`: the agent-independent client they call (the store, the config
  and the server kept in step). `internal/config`: `config.json`. `internal/adapters`: the agent adapter registry.
  `internal/procs`: process ancestry, to find the agent session a command runs under.
- `internal/protocol`: the wire types and the agent texts. `*_gen.go` are generated; never edit them.
- `internal/ops`: what each agent command does, answering with an ordered JSON object (`--json` prints it).
  `internal/core`: what the commands do with the store and the server. `internal/ojson`: ordered JSON objects (tool answers keep Python's field order).
- `internal/protocolgen`, `tools/gen`: the generator (`go run ./tools/gen`).
- `internal/deliverables`: the Deliverables check (a port of the server's, run before anything is sent), tested
  against the shared vectors in `protocol/deliverables/vectors.json`.
- `internal/artifacts`: reading a file to publish (at most 5 MB; larger ones are published as links), its kind
  and media type, and pulling a stored version back.
- `internal/sendlock`: the outbox's sender lock (one sender per outbox across processes).
- `internal/syncer`: the outbox flush (in order, batches, human acts alone), and pulling events into the replica.
- `internal/live`: the background sync (`teamflow sync`): live sockets, the `sync.poll` fallback, the one-copy lock.
- `bench/`: what an agent waits for, measured in CI: the prompt hook, a fresh `teamflow` process on a warm store,
  p95 under 30 ms over 200 runs, with the background sync running and with the hook starting it.
- `protocol/`: the protocol exported from the Teamflow server: `schema.json`, `methods.json`, golden messages,
  Deliverables test vectors, hook fixtures and agent texts (instructions, `/teamf`, the commands). `VERSION` names the server commit and
  protocol version it came from.

`go test ./...` decodes and re-encodes every golden message through the Go types, so a protocol change the client
does not follow fails the build.

## Developer notes

- **Build values to send with their constructors.** Every protocol type has `protocol.New<Type>()`, which holds
  the schema's defaults. A field left at Go's zero value is sent as that value, not as the server's default, so
  `protocol.ArtifactArchive{Artifact: id}` would send `"archived": false` (an un-archive), while
  `a := protocol.NewArtifactArchive(); a.Artifact = id` sends `"archived": true`. Decoding fills missing fields
  with their defaults by itself.
- **Unknown input is tolerated.** Unknown fields are ignored, and a union variant this client does not know (an
  event type from a newer server) decodes to its `Unknown*` type, never an error. A signal body sent without its
  `type` is matched only when exactly one variant fits its fields; otherwise it stays unknown.
- `protocol.Request` always encodes `"jsonrpc": "2.0"`.
- **A human act is never sent in an array body**, not even a one-element array: the server counts any array as a
  batch and refuses a human act inside one. Human acts are listed in the schema's `x-constants.HUMAN_ACTS`
  (generated as `protocol.HumanActsMethods` and `protocol.HumanActsVerdictSignalKinds`): verdicts (`APPROVE`,
  `REJECT` signals) and drift resolve and dismiss. The outbox (`syncer.Flush`) sends each with a single call,
  never `Batch`; queue new ones through `core` (see [How signals are sent](how-it-works.md#how-signals-are-sent)), never call them directly.
- **A new server method must be classified** in `core.SharedWrites` or `core.NotSharedWrites`
  (`internal/core/outbox.go`); a test fails until it is.
- List constants in `x-constants` (lists of strings only) are generated as `var`s of `[]string`; never change
  them.
- After changing `protocol/`, run `go run ./tools/gen`; CI fails if the generated code is stale.

## The README's clip

`docs/demo.gif` is recorded with [VHS](https://github.com/charmbracelet/vhs) from `docs/demo.tape`, against a local
Teamflow server with a demo account made by `docs/demo-setup.sh` (the steps are at the top of the tape).
