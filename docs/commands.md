# Commands

```text
teamflow login -e EMAIL [-s SERVER] [--password-stdin]   sign in (the password is asked for without echo)
teamflow logout | status | whoami | inbox | projects
teamflow install -a AGENT… -e EMAIL [-s SERVER]          sign in and set Teamflow up in your agents
teamflow uninstall [-a AGENT…] [--purge]
teamflow stop                                            stop sharing the current agent session
teamflow approve SG_ID [NOTE] | reject SG_ID NOTE        your verdict on an approval request
teamflow team …                                          behind the agent's /teamf command
teamflow signal emit --file F | get SG_ID | list [--topic T …] | cite SG_ID…
teamflow project get | create --file F | add-member M… | title M T | remove-member M | plan … | history | request-update
teamflow topic create T --description D | update T [--name …] [--close|--reopen] | delete T [--yes]
teamflow artifact write|link|pull|archive|list …
teamflow deliverables [--project P] | validate --file F | publish|update-milestone|rename --base-version N --file F
teamflow deliverables archive [--restore] | delete | history [--project P] [--yes]
teamflow action result AI_ID DONE|PARTIAL|BLOCKED --summary S | action revoke AI_ID REASON
teamflow drift list [--status S] | resolve|dismiss DR_ID NOTE [--project P] [--yes]
teamflow org members|add-member|remove-member|title …
teamflow profile [--name NAME] [--alias ALIAS] | profile avatar FILE | profile avatar --remove
teamflow tasks [--project P] [--kind K] [--state S]
teamflow log [--all] [--project P]
```

The commands an agent runs are listed, with their exact usage, in `protocol/agent/commands.json`;
`teamflow <command> --help` prints that usage line, and for a command that takes a JSON document (`--file`) its
exact shape. Every one of them takes `--json` and then prints one JSON object (a list answer under `result`, a
failure under `error`). Every one's output ends with what is new for the agent session it runs under: approval
requests waiting for the person and the session's other notices, each told once, even to commands running at the
same time. Plain output ends with a block between `--- Teamflow: new for this session ---` and `--- end of
Teamflow news ---`; `--json` adds `approval_requests` and `notices` fields. A failure carries them too.

Every action the Teamflow web app has is a command here. The server decides who may do what. Consequential
commands (delete, archive, remove a member, rewrite the plan, settle a drift) ask on the terminal, or take `--yes`.
What a command records inside an agent session is the agent's (actor AGENT); outside one, yours.

`SERVER` defaults to `https://teamflow.10l.ink`. A password is never a command-line argument. A verdict is always
recorded as yours, whether you run it, the web app records it, or your agent records it on your explicit words.
