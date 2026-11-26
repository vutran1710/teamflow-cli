#!/bin/sh
# Prepares a throwaway world for docs/demo.tape: a local Teamflow server with a demo account (you@example.com),
# an org and a project, and an empty home whose Teamflow config points at that server (so the clip needs no -s).
# Nothing outside DIR is touched.
#
#   TEAMFLOW_SERVER_CLI=/path/to/teamflow-server sh docs/demo-setup.sh DIR    # prints the server's pid
#   DEMO_DIR=DIR vhs docs/demo.tape
#   kill <pid>; rm -rf DIR
set -eu
D=${1:?usage: demo-setup.sh DIR}
CLI=${TEAMFLOW_SERVER_CLI:?set TEAMFLOW_SERVER_CLI to the server admin CLI}
PASSWORD=demo-password-1
PORT=${DEMO_PORT:-8711}
URL=http://127.0.0.1:$PORT

mkdir -p "$D/data" "$D/home/.claude" "$D/home/.teamflow" "$D/home/.local/bin"
chmod 700 "$D/home/.teamflow"
export TEAMFLOW_DATA_DIR="$D/data"
"$CLI" init >/dev/null
printf '%s\n' "$PASSWORD" | "$CLI" account add you@example.com --name You --password-stdin >/dev/null
"$CLI" org add Acme --admin you@example.com >/dev/null
"$CLI" run --port "$PORT" >"$D/server.log" 2>&1 &
echo $! >"$D/server.pid"
i=0
until curl -fs "$URL/health" >/dev/null 2>&1; do
  i=$((i + 1)); [ $i -lt 80 ] || { echo "server did not start" >&2; exit 1; }
  sleep 0.25
done

rpc() { curl -fs "$URL/rpc" -H 'content-type: application/json' ${2:+-H "authorization: Bearer $2"} -d "$1"; }
KEY=$(rpc "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"auth.login\",\"params\":{\"email\":\"you@example.com\",\"password\":\"$PASSWORD\"}}" \
  | sed -n 's/.*"auth_key": *"\([^"]*\)".*/\1/p')
rpc '{"jsonrpc":"2.0","id":2,"method":"project.create","params":{"org":"Acme","name":"checkout","topics":[{"name":"payments","description":"How we take money"},{"name":"api","description":"The public API"}]}}' "$KEY" >/dev/null

# The server the clip signs in to, with no key yet: `teamflow install` without -s uses it.
printf '{\n  "server": "%s",\n  "auth_key": "",\n  "email": ""\n}\n' "$URL" >"$D/home/.teamflow/config.json"
chmod 600 "$D/home/.teamflow/config.json"
cat "$D/server.pid"
