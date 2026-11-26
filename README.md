# teamflow

The command line for [Teamflow](https://teamflow.10l.ink): your coding agent records the team's decisions as it
works, and the important calls reach the approver at once. Read and approve them in the
[web app](https://teamflow.10l.ink/ui/) or right here.

![Install teamflow, set up Claude Code, list your projects](docs/demo.gif)

## Install

macOS and Linux (amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh
```

The installer checks the release's checksum, and its signature when `minisign` is installed
([verify the download](docs/VERIFY.md)), and puts `teamflow` in `~/.local/bin`, which must be on your PATH
(it tells you the line to add if not).
More options, and building from source: [docs/install.md](docs/install.md).

## Set up your agent

```sh
teamflow install -a claude-code -e you@example.com    # or -a copilot; cursor and antigravity are experimental
teamflow uninstall                                    # takes out exactly what install added
```

Install signs you in, adds Teamflow's hooks, instructions, permission rules and the `/teamf` command to the
agent's config (your own settings stay as they are), and starts the background sync. Then, in a session, run
`/teamf project <name>` to choose the project the agent works on.

## Important commands

| Command | What it does |
| --- | --- |
| `teamflow status` | Server, sign-in, outbox, sessions and agents on this machine |
| `teamflow projects` | Every project you are in, across your orgs |
| `teamflow project get` | The project: topics, members, plan and instruction |
| `/teamf project <name> [topic …]` | In an agent session: share it with a project (`teamflow team project`) |
| `teamflow team approvals` | The approval requests waiting for you |
| `teamflow approve <sg_id> [note]` | Approve a signal you were asked to approve |
| `teamflow reject <sg_id> <note>` | Reject it, saying why |
| `teamflow signal emit --file <f>` | Record decisions, questions, notes (`--help` shows the JSON) |
| `teamflow signal list` / `signal get <sg_id>` | Search the project's signals / read one |
| `teamflow inbox` | This session's notices |
| `teamflow deliverables` | Milestones and actions, with owner, due date and status |
| `teamflow action result <ai_id> DONE --summary <s>` | Report on an action (or `PARTIAL`, `BLOCKED`) |
| `teamflow artifact write <file>` / `artifact pull <id>` | Publish a file teammates rely on / save one |
| `teamflow outbox` | What waits to be sent, in order |
| `teamflow sync --status` | The background sync (`--stop` stops it) |
| `teamflow self-update` | Update to the newest release (signature checked first) |

Every command takes `--json`; `teamflow <command> --help` gives the details. The full list: [docs/commands.md](docs/commands.md).

## Docs

- [docs/agents.md](docs/agents.md): what install writes for Claude Code, Copilot, Cursor and Antigravity, and its limits.
- [docs/how-it-works.md](docs/how-it-works.md): how signals are sent (with diagrams), the background sync, what it sends and where.
- [docs/development.md](docs/development.md): repository layout and developer notes.
- [docs/VERIFY.md](docs/VERIFY.md): verifying a release.

## License

MIT; see [LICENSE](LICENSE).
