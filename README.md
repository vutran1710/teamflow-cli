# teamflow

The command line for [Teamflow](https://teamflow.10l.ink): your coding agent records the team's decisions as it
works, and the important calls reach the approver at once. Read and approve them in the
[web app](https://teamflow.10l.ink/ui/) or right here.

![Install teamflow with one line and answer its questions, then choose a project in Claude Code and see where it stands](docs/demo.gif)

## Install and set up

macOS and Linux (amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh
```

It checks the release's checksum, and its signature when `minisign` is installed ([verify](docs/VERIFY.md)), puts
`teamflow` in `~/.local/bin`, then asks for your email and which agents to set up (Claude Code, Copilot CLI; Cursor
CLI and Antigravity CLI are experimental) and signs you in. Your own agent settings stay as they are.

Later, or to add another agent: `teamflow install -a <agent>` (`claude-code`, `copilot`; `cursor` and `antigravity`
are experimental). To take it out: `teamflow uninstall`.
More options, scripted installs and building from source: [docs/install.md](docs/install.md).
In a session, `/teamf project <name>` chooses the project the agent works on, and `/teamf status` shows it at a glance.

## Important commands

| Command | What it does |
| --- | --- |
| `teamflow status` | Server, sign-in, outbox, sessions and agents on this machine |
| `teamflow projects` | Every project you are in, across your orgs |
| `teamflow project status` | The project at a glance: progress, milestones, what needs you, what is due soon |
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
| `teamflow self-update` | Update to the newest release (signature checked first); `/teamf upgrade` in a session |
| `teamflow config auto-update off` | Stop the background sync installing new releases on its own (`on` again) |

Every command takes `--json`; `teamflow <command> --help` gives the details. The full list: [docs/commands.md](docs/commands.md).

## Docs

- [docs/agents.md](docs/agents.md): what install writes for Claude Code, Copilot, Cursor and Antigravity, and its limits.
- [docs/how-it-works.md](docs/how-it-works.md): what happens when your agent records something, what is sent and where.
- [docs/development.md](docs/development.md): build, test and release.
- [docs/VERIFY.md](docs/VERIFY.md): verifying a release.

## License

MIT; see [LICENSE](LICENSE).
