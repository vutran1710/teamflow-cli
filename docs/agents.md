# Agents

## Agents use the command line

There is no MCP server: every agent runs `teamflow` commands in its shell (`protocol/agent/commands.json` lists
them), with one agent-neutral instruction text (`protocol/agent/instructions.md`) placed where each agent reads
instructions. Each command reads the sign-in and keys from disk and finds the agent session it runs under.

**Release notes (vs. the Python client):** the placeholder `teamflow tui` is dropped. The local store is new
(`~/.teamflow/store.db`); a Python-era `client.sqlite` is left alone and can be deleted, so sign in once more with
`teamflow login`.

## Claude Code

`teamflow install -a claude-code` writes only Teamflow's own entries ($CLAUDE_CONFIG_DIR, else `~/.claude`):

- `settings.json`: hooks for SessionStart, UserPromptSubmit, PreToolUse, Stop and SessionEnd,
  `permissions.allow` rules `Bash(teamflow <command>:*)` for each work command (the same by the binary's path), and
  `permissions.deny` rules for the Teamflow home (backed up once as `settings.json.teamflow-backup`). Setup commands
  (login, logout, install, uninstall, self-update, sync) keep Claude Code's prompt;
- `CLAUDE.md`: the instruction text, word for word, between `<!-- teamflow:begin … -->` and `<!-- teamflow:end -->`;
- `commands/teamf.md`: the `/teamf` command;
- `.teamflow-install.json`: what install added, so `teamflow uninstall` takes out exactly that.

No MCP server is registered. An older install's `teamflow` MCP server in `.claude.json` is taken out by install and
by uninstall; a `.claude.json` without one is never touched.

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

**Cursor CLI and Antigravity CLI are experimental:** not yet verified on a real install. `teamflow install -a all`
and the list of agents found on the machine leave them out; `-a cursor` or `-a antigravity` sets one up, with a
warning saying so. Claude Code and Copilot CLI are set up by `-a all` when found.

These agents use the `teamflow` commands; no MCP server is registered for them. Each install writes only Teamflow's
own entries or files, records what it added in `.teamflow-install.json` beside them (with a copy of each file it
edited, mode 0600, so uninstall gives your file back byte for byte), refuses a malformed, read-only or wrongly
shaped file before writing anything, and never fails your agent's step.

- **Copilot CLI** (`$COPILOT_HOME`, else `~/.copilot`): `hooks/teamflow.json` (sessionStart, userPromptSubmitted,
  preToolUse, postToolUse, agentStop, sessionEnd), `copilot-instructions.md` (the instruction block) and
  `skills/teamf/SKILL.md` (`/teamf`, `allowed-tools: shell(teamflow:*)`). Copilot keeps no allow or deny rules in
  your config, so the preToolUse hook answers, without opening the Teamflow home: it refuses tool calls that name
  the home; allows one plain `teamflow <work command>` with no prompt; and answers "ask" for any other teamflow
  command (setup: install, uninstall, login, logout, self-update, hook, sync, outbox; or anything chained, piped,
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
