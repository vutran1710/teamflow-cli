# teamflow-cli

The Teamflow client: one static binary, `teamflow`, that holds the command line, the MCP server your agent talks
to, and the agents' hooks. It records what your agents decide and do into a Teamflow project, and tells each
agent session what changed since its last step.

**Status: early.** This repo holds the wire protocol (Go types generated from the schema the Teamflow server
exports, checked against golden messages), the local store and sync, the command line and the MCP server. The
agent adapters (Claude Code, Cursor CLI, Antigravity CLI, Copilot CLI), the background sync and self-update land
here as they are built. The first release is tagged once the client passes the server's
end-to-end suite.

## Install

Releases are not published yet. When they are, each release on this repo's Releases page carries a binary for
macOS and Linux (amd64 and arm64), a SHA-256 checksum file, and a minisign (ed25519) signature of that file.

To build from source (Go 1.26 or later):

```sh
go install github.com/vutran1710/teamflow-cli/cmd/teamflow@latest
teamflow --version
```

## Commands

```text
teamflow login -e EMAIL [-s SERVER] [--password-stdin]   sign in (the password is asked for without echo)
teamflow logout | projects [--json] | status
teamflow install -a AGENT… -e EMAIL [-s SERVER]          sign in and set Teamflow up in your agents
teamflow uninstall [-a AGENT…] [--purge]
teamflow stop                                            stop sharing the current agent session
teamflow approve SG_ID [NOTE] | reject SG_ID NOTE        your verdict on an approval request
teamflow team …                                          behind the agent's /teamf command
teamflow topic delete PROJECT TOPIC [--yes]
teamflow artifact write|link|pull|archive|list …
teamflow deliverables [--project P] | deliverables validate FILE [--json]
teamflow deliverables archive [--restore] | delete | history [--project P] [--yes]
teamflow drift resolve|dismiss DR_ID NOTE [--project P] [--yes]
teamflow project title MEMBER TITLE | remove-member MEMBER | plan [--plan|--plan-file …] [--project P]
teamflow org members|add-member|remove-member|title …
teamflow profile [--name NAME] [--alias ALIAS] | profile avatar FILE | profile avatar --remove
teamflow tasks [--project P] [--kind K] [--state S]
teamflow log [--all] [--project P]
teamflow mcp                                             the MCP server (started by your agent, over stdio)
```

Every action the Teamflow web app has is a command here and an MCP tool (pictures are command-line only: they
need a file). The server decides who may do what. Consequential commands (delete, archive, remove a member,
rewrite the plan, settle a drift) ask on the terminal, or take `--yes`.

`SERVER` defaults to `https://teamflow.vutr.io`. A password is never a command-line argument. A verdict is always
recorded as yours, whether you run it, the web app records it, or your agent records it on your explicit words.

## The MCP server

`teamflow mcp` serves 44 tools (`protocol/agent/tools.json`) to any MCP agent, with one agent-neutral instruction
text (`protocol/agent/instructions.md`) as its instructions. The tools the Python client had keep its input
schemas exactly. Each call reads the sign-in and keys from disk and finds the agent session it runs under. Every
result carries what is new for the session: approval requests waiting for you, and the session's other notices, once.

**Release notes (vs. the Python client):** the placeholder `teamflow tui` is dropped. The local store is new
(`~/.teamflow/store.db`); a Python-era `client.sqlite` is left alone and can be deleted, so sign in once more with
`teamflow login`.

## Claude Code

`teamflow install -a claude-code` writes only Teamflow's own entries ($CLAUDE_CONFIG_DIR, else `~/.claude`):

- `.claude.json` (`$CLAUDE_CONFIG_DIR/.claude.json` or `~/.claude.json`): the MCP server `teamflow`;
- `settings.json`: hooks for SessionStart, UserPromptSubmit, PreToolUse, Stop and SessionEnd, and
  `permissions.deny` rules for the Teamflow home (backed up once as `settings.json.teamflow-backup`);
- `CLAUDE.md`: the instruction text, word for word, between `<!-- teamflow:begin … -->` and `<!-- teamflow:end -->`;
- `commands/teamf.md`: the `/teamf` command;
- `.teamflow-install.json`: what install added, so `teamflow uninstall` takes out exactly that.

A malformed or read-only file stops the install before anything is written. Your own settings, hooks, rules and
instructions stay byte for byte; a Python-era install's entries are replaced, never doubled. A hook never fails
your agent's step: on any trouble it adds nothing and writes `~/.teamflow/hook.log`. The hook before each tool
works from the local copy and goes to the network at most once every 10 seconds per project (1-second timeout;
none for 30 seconds after a failure; none while the background sync is live).

**The deny rules' limits.** The rules (`Read(~/.teamflow)`, `Read(~/.teamflow/**)`, `Edit(…)` the same, and
`Bash(*~/.teamflow*)`, `Bash(*$HOME/.teamflow*)`, `Bash(*${HOME}/.teamflow*)`, `Bash(*<the home's full path>*)`,
`Bash(*.teamflow/*)`, `Bash(*TEAMFLOW_HOME*)`) keep Claude Code's own file tools out of the Teamflow home and refuse
shell commands that name it. They are a guard rail, not a sandbox. Bash rules match the command's text: a command
that reaches the folder without naming it that way (`cd ~ && cat .team*/config.json`, a variable, a script, a glob such as
`~/.team*`, or a case variant of the path on a case-insensitive disk) is not stopped by them. Read and Edit rules
apply to Claude Code's built-in tools and the shell commands it recognises, not to programs that open files
themselves. For a hard boundary use Claude Code's sandbox. The instruction text also tells every agent the home is
not theirs.

## Copilot CLI, Cursor CLI and Antigravity CLI

These agents use the `teamflow` commands; no MCP server is registered for them. Each install writes only Teamflow's
own entries or files, records what it added in `.teamflow-install.json` beside them (with a copy of each file it
edited, mode 0600, so uninstall gives your file back byte for byte), refuses a malformed, read-only or wrongly
shaped file before writing anything, and never fails your agent's step.

- **Copilot CLI** (`$COPILOT_HOME`, else `~/.copilot`): `hooks/teamflow.json` (sessionStart, userPromptSubmitted,
  preToolUse, postToolUse, agentStop, sessionEnd), `copilot-instructions.md` (the instruction block) and
  `skills/teamf/SKILL.md` (`/teamf`, `allowed-tools: shell(teamflow:*)`). Copilot keeps no allow or deny rules in
  your config, so the preToolUse hook answers, without opening the Teamflow home: it refuses tool calls that name
  the home; allows one plain `teamflow <work command>` with no prompt; and answers "ask" for any other teamflow
  command (setup: install, uninstall, login, logout, self-update, hook, sync until the background sync is built; or anything chained, piped,
  redirected, substituted or expanded: `; & | < > $ ` ( ) \ * ? [ ] { } ~`). Copilot's prompt hook cannot add
  context, so news arrives at session start, after each tool, and at the turn's end.
- **Cursor CLI** (`~/.cursor`): `hooks.json` (sessionStart, beforeSubmitPrompt, beforeShellExecution, postToolUse,
  stop, sessionEnd, beside your own hooks), `cli-config.json` (`permissions.allow`: `Shell(teamflow)` and
  `Shell(<binary>)`; `permissions.deny`: `Read(<home>)`, `Read(<home>/**)`, `Write(…)` the same),
  `rules/teamflow.mdc` (the instruction text, `alwaysApply`; the sessionStart hook gives it too) and
  `skills/teamf/SKILL.md`. The beforeShellExecution hook (answering without opening the home) refuses shell
  commands naming the home, and answers "ask" for any teamflow command that is not one plain work command, whatever
  the `Shell(teamflow)` rule matches.
- **Antigravity CLI** (`~/.gemini`): `config/hooks.json` (a hook named `teamflow`: SessionStart, PreInvocation,
  Stop), `config/AGENTS.md` (the instruction block), `config/skills/teamf/SKILL.md`, and
  `antigravity-cli/settings.json` (`permissions.allow`: `command(teamflow <work command>)` and the same by the
  binary's path, one rule per work command, so setup commands keep the prompt;
  `permissions.deny`: `read_file(<home>)`, `write_file(<home>)` and `command(regex:…)` rules for commands naming
  the home).

The same limits as Claude Code's deny rules apply: they stop what names the folder, not a command that reaches it
another way. The CLI itself never reads a file in the Teamflow home for a command (`--plan-file`,
`--instruction-file`, `deliverables validate`, `artifact write`, `profile avatar`) nor writes one there
(`artifact pull -o`), however the path names it: the check is on the resolved file, by identity.

## What it sends, and where

The client talks to one place only: the Teamflow server you sign in to, over HTTPS (`/rpc`, JSON-RPC) and a
WebSocket (`/live`). It sends nothing to anyone else: no telemetry, no analytics, no crash reports.

What goes to that server:

- **Signing in:** your email and password, once; the server answers with an account key.
- **On every call:** your key in the `Authorization` header, and a `Teamflow-Client` header with this client's
  name, version and protocol number.
- **What you and your agents record:** signals (decisions, key points, comments, results), topics, artifacts
  (only the files an agent or you choose to publish), deliverables and their updates, and your approvals or
  rejections (marked when given through your agent). Each carries the agent session id it came from.
- **Keeping up to date:** your position (cursor) in each project's event log, to fetch what changed.

What stays on your machine:

- Your prompts and code.
- Your keys and the local copy of your projects, in `~/.teamflow` (or `$TEAMFLOW_HOME`): the folder is mode 0700
  and its files 0600 (`config.json` holds the server, your email and the auth key; `store.db` the rest). Keys never appear in command lines, logs or URLs.

## Repository layout

- `cmd/teamflow`: the binary.
- `internal/cli`: the commands. `internal/core`: the agent-independent client they call (the store, the config
  and the server kept in step). `internal/config`: `config.json`. `internal/adapters`: the agent adapter registry.
  `internal/procs`: process ancestry, to find the agent session a command runs under.
- `internal/protocol`: the wire types and the agent texts. `*_gen.go` are generated; never edit them.
- `internal/mcpserver`: the MCP server and its tools. `internal/core`: what the commands and tools do with the
  store and the server. `internal/ojson`: ordered JSON objects (tool answers keep Python's field order).
- `internal/protocolgen`, `tools/gen`: the generator (`go run ./tools/gen`).
- `internal/deliverables`: the Deliverables check (a port of the server's, run before anything is sent), tested
  against the shared vectors in `protocol/deliverables/vectors.json`.
- `internal/artifacts`: reading a file to publish (at most 5 MB; larger ones are published as links), its kind
  and media type, and pulling a stored version back.
- `protocol/`: the protocol exported from the Teamflow server: `schema.json`, `methods.json`, golden messages,
  Deliverables test vectors, MCP tool list, hook fixtures and agent texts. `VERSION` names the server commit and
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
  `REJECT` signals) and drift resolve and dismiss. Send each with `api.Client.Call`, never `Batch`.
- List constants in `x-constants` (lists of strings only) are generated as `var`s of `[]string`; never change
  them.
- After changing `protocol/`, run `go run ./tools/gen`; CI fails if the generated code is stale.

## License

MIT; see [LICENSE](LICENSE).
