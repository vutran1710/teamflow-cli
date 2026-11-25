# Hook fixtures

Each `<event>.<case>.json` is one recorded call of Teamflow's Claude Code hook (recorded once from the reference
implementation, then frozen): the hook `event`, the `stdin` it was given, the `stdout` it answered, the `scenario` (the
state it ran in) and its `layout`:

- `guard`: the folder layout in `scenario` ({root}, {home}; `env` gives TEAMFLOW_HOME and HOME). Replace {root} and
  {home} in `stdin` with the real folders before running the hook. No store is needed.
- `none`: a Teamflow home with no sign-in.
- `shared`: an in-process server with the state `scenario` describes; reproduce that state to replay it.

`compare` says how an implementation's stdout is checked against `stdout`. The one rule, `json-normalised`:

1. An empty `stdout` means the hook writes nothing at all: the other side must be empty too.
2. Otherwise both are parsed as JSON and compared as values (key order, spacing and escapes such as `\u00ab` do not
   matter: Python's json.dumps and Go's encoding/json differ there). Strings inside must be equal exactly.
3. Before comparing, both sides (stdin first, then stdout) are normalised: each id (`<pj|tp|sg|mb|tk|dr|ac|og|ms|ai>_`
   followed by 26 Crockford base32 characters) becomes `<prefix>_` and its number among the ids of that prefix, in
   order of first appearance, zero-padded to 26 digits (`sg_00000000000000000000000001`); each RFC 3339 timestamp
   becomes 2026-01-15T09:00:00Z plus its rank in seconds among the distinct timestamps, in time order. The fixtures
   are already normalised this way.
