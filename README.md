# teamflow-cli

The Teamflow client: one static binary, `teamflow`, that holds the command line, the MCP server Claude Code talks
to, and the Claude Code hooks. It records what your agents decide and do into a Teamflow project, and tells each
agent session what changed since its last step.

**Status: early.** This repo currently holds the wire protocol (Go types generated from the schema the Teamflow
server exports, checked against golden messages) and a `teamflow --version` stub. The commands, the MCP server
and the hooks land here as they are built. The first release is tagged once the client passes the server's
end-to-end suite.

## Install

Releases are not published yet. When they are, each release on this repo's Releases page carries a binary for
macOS and Linux (amd64 and arm64), a SHA-256 checksum file, and a minisign (ed25519) signature of that file.

To build from source (Go 1.26 or later):

```sh
go install github.com/vutran1710/teamflow-cli/cmd/teamflow@latest
teamflow --version
```

## What it sends, and where

The client talks to one place only: the Teamflow server you sign in to, over HTTPS (`/rpc`, JSON-RPC) and a
WebSocket (`/live`). It sends nothing to anyone else: no telemetry, no analytics, no crash reports.

What goes to that server:

- **Signing in:** your email and password, once; the server answers with an account key.
- **On every call:** your key in the `Authorization` header, and a `Teamflow-Client` header with this client's
  name, version and protocol number.
- **What you and your agents record:** signals (decisions, key points, comments, results), topics, artifacts
  (only the files an agent or you choose to publish), deliverables and their updates, and the approvals or
  rejections you type yourself. Each carries the Claude Code session id it came from.
- **Keeping up to date:** your position (cursor) in each project's event log, to fetch what changed.

What stays on your machine:

- Your prompts and code. The hooks read your prompt locally to recognise your own `/teamf` commands; it is not
  uploaded.
- Your keys and the local copy of your projects, in `~/.teamflow` (or `$TEAMFLOW_HOME`): the folder is mode 0700
  and its files 0600. Keys never appear in command lines, logs or URLs.

## Repository layout

- `cmd/teamflow`: the binary.
- `internal/protocol`: the wire types. `*_gen.go` are generated; never edit them.
- `internal/protocolgen`, `tools/gen`: the generator (`go run ./tools/gen`).
- `protocol/`: the protocol exported from the Teamflow server: `schema.json`, `methods.json`, golden messages,
  Deliverables test vectors, MCP tool list, hook fixtures and agent texts. `VERSION` names the server commit and
  protocol version it came from.

`go test ./...` decodes and re-encodes every golden message through the Go types, so a protocol change the client
does not follow fails the build.

## License

MIT; see [LICENSE](LICENSE).
