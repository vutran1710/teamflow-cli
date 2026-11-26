#!/bin/sh
# Prepares a throwaway world for docs/demo.tape, all under DIR and the clip's home (/tmp/you); nothing else is
# touched (never your ~/.teamflow, ~/.claude, ~/.claude.json, ~/.local/bin or your agents' configs):
#   - a local Teamflow server with a demo account (you@example.com, "Sam"), an org (Acme) and a project (checkout);
#   - a local "Releases page" holding this checkout's teamflow, signed with a throwaway minisign key, and a copy of
#     install.sh carrying that key (so the installer's signature check runs, against the demo key);
#   - the clip's home: Claude Code's first-run state (onboarding done, the demo folder trusted) and, on macOS,
#     Library/Keychains linked to yours, where Claude Code finds its sign-in (as e2e/run.py gives its people);
#     ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, when set, are used instead.
#
#   TEAMFLOW_SERVER_CLI=/path/to/teamflow-server sh docs/demo-setup.sh DIR
#   vhs docs/demo.tape
#   sh docs/demo-setup.sh --clean DIR
set -eu
HOME_DIR=/private/tmp/you
[ "$(uname -s)" = Darwin ] || HOME_DIR=/tmp/you

if [ "${1:-}" = --clean ]; then
  D=${2:?usage: demo-setup.sh --clean DIR}
  HOME="$HOME_DIR" "$HOME_DIR/.local/bin/teamflow" sync --stop >/dev/null 2>&1 || true
  for p in "$D/server.pid" "$D/release.pid"; do [ -f "$p" ] && kill "$(cat "$p")" 2>/dev/null || true; done
  [ -f "$HOME_DIR/.demo-home" ] && rm -rf "$HOME_DIR"
  rm -rf "$D"
  exit 0
fi

D=${1:?usage: demo-setup.sh DIR}
CLI=${TEAMFLOW_SERVER_CLI:?set TEAMFLOW_SERVER_CLI to the server admin CLI}
REPO=$(cd "$(dirname "$0")/.." && pwd)
PASSWORD=demo-password-1
PORT=${DEMO_PORT:-8711}
REL_PORT=${DEMO_RELEASE_PORT:-8712}
URL=http://localhost:$PORT
TAG=${DEMO_TAG:-v0.2.0}

if [ -e "$HOME_DIR" ] && [ ! -f "$HOME_DIR/.demo-home" ]; then
  echo "$HOME_DIR exists and was not made by demo-setup.sh; move it away first" >&2
  exit 1
fi
rm -rf "$HOME_DIR"
mkdir -m 700 "$HOME_DIR"
: >"$HOME_DIR/.demo-home"
mkdir -p "$D/data" "$D/release/download/$TAG" "$HOME_DIR/checkout" "$HOME_DIR/.claude"

# The server, its demo account, org and project.
export TEAMFLOW_DATA_DIR="$D/data"
"$CLI" init >/dev/null
printf '%s\n' "$PASSWORD" | "$CLI" account add you@example.com --name Sam --password-stdin >/dev/null
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
rpc '{"jsonrpc":"2.0","id":3,"method":"auth.logout","params":{}}' "$KEY" >/dev/null || true

# The release: this checkout's teamflow, its checksum, signed with a throwaway key for $TAG.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
asset="teamflow_${os}_${arch}.tar.gz"
mkdir -p "$D/build"
(cd "$REPO" && go build -ldflags "-X main.version=$TAG" -o "$D/build/teamflow" ./cmd/teamflow)
(cd "$D/build" && tar -czf "$D/release/download/$TAG/$asset" teamflow)
(cd "$D/release/download/$TAG" && shasum -a 256 "$asset" >checksums.txt)
minisign -G -W -p "$D/demo.pub" -s "$D/demo.key" >/dev/null
minisign -S -s "$D/demo.key" -m "$D/release/download/$TAG/checksums.txt" \
  -t "$(printf 'timestamp:%s\tfile:checksums.txt\ttag:%s' "$(date +%s)" "$TAG")" >/dev/null
PUB=$(tail -1 "$D/demo.pub")
sed "s|^\tPUBKEY=\".*\"$|\tPUBKEY=\"$PUB\"|" "$REPO/install.sh" >"$HOME_DIR/.demo-install.sh"
grep -q "PUBKEY=\"$PUB\"" "$HOME_DIR/.demo-install.sh"
(cd "$D/release" && exec python3 -m http.server "$REL_PORT" --bind 127.0.0.1 >"$D/release.log" 2>&1) &
echo $! >"$D/release.pid"

# The clip's Teamflow points at the demo server (no key yet), so `teamflow install` needs no -s.
mkdir -m 700 "$HOME_DIR/.teamflow"
printf '{\n  "server": "%s",\n  "auth_key": "",\n  "email": ""\n}\n' "$URL" >"$HOME_DIR/.teamflow/config.json"
chmod 600 "$HOME_DIR/.teamflow/config.json"

# Claude Code's first run, done: onboarding, a theme, the demo folder trusted.
python3 - "$HOME_DIR" "$(claude --version 2>/dev/null | cut -d' ' -f1)" <<'EOF'
import json, sys
home = sys.argv[1]
folders = {f"{home}/checkout", f"{home.removeprefix('/private')}/checkout"}
trusted = {"hasTrustDialogAccepted": True, "hasCompletedProjectOnboarding": True, "allowedTools": []}
cfg = {"hasCompletedOnboarding": True, "theme": "dark", "projects": {f: trusted for f in folders},
       # one-time notices, already seen: the clip shows Claude Code, not its news
       "hasSeenAutoDefaultNotice": True, "announcementImpressions": {"fable-5-2-launch": 99},
       "lastReleaseNotesSeen": sys.argv[2], "lastClawdEntranceVersion": sys.argv[2], "numStartups": 20,
       "passesUpsellSeenCount": 99, "hasVisitedPasses": True, "officialMarketplaceAutoInstallAttempted": True,
       "officialMarketplaceAutoInstalled": True}
open(f"{home}/.claude.json", "w").write(json.dumps(cfg))
EOF
# Claude Code and minisign, alone on the clip's PATH (never your ~/.local/bin, which may hold another teamflow;
# no other agent, so the clip finds Claude Code only).
mkdir "$HOME_DIR/.demo-bin"
ln -s "$(command -v claude)" "$HOME_DIR/.demo-bin/claude"
ln -s "$(command -v minisign)" "$HOME_DIR/.demo-bin/minisign"
if [ -d "$HOME/Library/Keychains" ]; then
  mkdir "$HOME_DIR/Library"
  ln -s "$HOME/Library/Keychains" "$HOME_DIR/Library/Keychains"
fi
echo "ready: home $HOME_DIR, server $URL, releases http://127.0.0.1:$REL_PORT ($TAG)"
