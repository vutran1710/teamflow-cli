#!/bin/sh
# Prepares the clip's throwaway home for docs/demo.tape (/tmp/you); nothing else is touched (never your ~/.teamflow,
# ~/.claude, ~/.claude.json, ~/.local/bin or your agents' configs). The clip runs against the real Teamflow
# (https://teamflow.10l.ink) and installs the real release from GitHub, signed in as a demo account whose org already
# holds a project "checkout" with a plan and Deliverables. The home holds:
#   - Claude Code's first run, done (onboarding, a theme, ~/checkout trusted, one-time notices seen) and, on macOS,
#     Library/Keychains linked to yours, where Claude Code finds its sign-in (as e2e/run.py gives its people);
#     ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, when set, are used instead;
#   - Claude Code and minisign, alone on the clip's PATH (so the installer checks the release's signature);
#   - .demo-type-password: started in the tape's hidden setup, it waits for teamflow install's password prompt (echo
#     off) and types the demo account's password there from DEMO_PASSWORD_FILE, read only then; the password is never
#     in the tape, the clip or this home.
#
#   DEMO_PASSWORD_FILE=/path/to/password sh docs/demo-setup.sh
#   vhs docs/demo.tape
#   sh docs/demo-setup.sh --clean     # signs the clip's teamflow out, stops its background sync, removes the home
set -eu
HOME_DIR=/private/tmp/you
[ "$(uname -s)" = Darwin ] || HOME_DIR=/tmp/you

if [ "${1:-}" = --clean ]; then
  if [ -x "$HOME_DIR/.local/bin/teamflow" ]; then
    HOME="$HOME_DIR" "$HOME_DIR/.local/bin/teamflow" sync --stop >/dev/null 2>&1 || true
    HOME="$HOME_DIR" "$HOME_DIR/.local/bin/teamflow" logout >/dev/null 2>&1 || true
  fi
  [ -f "$HOME_DIR/.demo-home" ] && rm -rf "$HOME_DIR"
  exit 0
fi

PWFILE=${DEMO_PASSWORD_FILE:?set DEMO_PASSWORD_FILE to the file holding the demo account password}
[ -r "$PWFILE" ] || { echo "cannot read $PWFILE" >&2; exit 1; }
PWFILE=$(cd "$(dirname "$PWFILE")" && pwd)/$(basename "$PWFILE")

if [ -e "$HOME_DIR" ] && [ ! -f "$HOME_DIR/.demo-home" ]; then
  echo "$HOME_DIR exists and was not made by demo-setup.sh; move it away first" >&2
  exit 1
fi
rm -rf "$HOME_DIR"
mkdir -m 700 "$HOME_DIR"
: >"$HOME_DIR/.demo-home"
mkdir -p "$HOME_DIR/checkout" "$HOME_DIR/.claude"

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

# The password typist: waits (up to 5 minutes) for the terminal to be in teamflow install's password mode (echo off,
# lines on: the agents list is raw, the email and the shell echo), then types the password and Enter into it
# (TIOCSTI, the clip's own terminal). It ignores SIGTTOU, as it runs in the background.
cat >"$HOME_DIR/.demo-type-password" <<EOF
#!$(python3 -c 'import sys; print(sys.executable)') -I
import fcntl, os, signal, termios, time
signal.signal(signal.SIGTTOU, signal.SIG_IGN)
fd = os.open("/dev/tty", os.O_RDWR)
sti = getattr(termios, "TIOCSTI", 0x80017472)
def asking():
    lflag = termios.tcgetattr(fd)[3]
    return not lflag & termios.ECHO and lflag & termios.ICANON
end = time.monotonic() + 300
while time.monotonic() < end:
    if asking():
        time.sleep(0.8)
        if asking():
            with open("$PWFILE") as f:
                keys = f.read().strip("\\r\\n") + "\\r"
            for b in keys.encode():
                fcntl.ioctl(fd, sti, bytes([b]))
                time.sleep(0.03)
            break
    time.sleep(0.05)
EOF
chmod 700 "$HOME_DIR/.demo-type-password"
echo "ready: home $HOME_DIR"
