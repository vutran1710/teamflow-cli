# The README's clip: the one curl line, two questions and a password, then Claude Code asked for the projects.
# Re-record with charmbracelet VHS (and minisign installed); everything runs in a throwaway world:
#
#   TEAMFLOW_SERVER_CLI=/path/to/teamflow-server sh docs/demo-setup.sh "$DIR"   # demo server, release, home
#   vhs docs/demo.tape
#   sh docs/demo-setup.sh --clean "$DIR"
#
# The clip's home is /tmp/you (demo-setup.sh makes it): the clip never reads or writes your own home, agents or
# Teamflow sign-in. The curl line is answered from the demo's copy of install.sh (signed with the demo's key) and
# its local Releases page; Claude Code is the real one, signed in through your keychain, on a cheap model.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 22
Set Width 1150
Set Height 840
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 25ms
Set PlaybackSpeed 1.4

Hide
Type "env -i HOME=/tmp/you USER=$USER TERM=xterm-256color LANG=en_US.UTF-8 PATH=/tmp/you/.local/bin:/tmp/you/.demo-bin:/usr/bin:/bin bash --norc --noprofile"
Enter
Type "export PS1='$ ' CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 TEAMFLOW_VERSION=v0.2.0 TEAMFLOW_RELEASES_URL=http://127.0.0.1:8712"
Enter
Type `curl() { case "$*" in *raw.githubusercontent.com*install.sh*) cat ~/.demo-install.sh ;; *) command curl "$@" ;; esac; }`
Enter
Type `claude() { command claude --model haiku "$@"; } && cd ~/checkout && claude`
Enter
Sleep 8s
Ctrl+C
Ctrl+C
Sleep 1s
Type "cd ~ && clear"
Enter
Sleep 500ms
Show

Type "curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh"
Sleep 300ms
Enter
Wait+Screen@30s /Set up teamflow now/
Sleep 700ms
Type "y"
Enter
Wait+Screen@10s /Email:/
Sleep 300ms
Type "you@example.com"
Sleep 300ms
Enter
Wait+Screen@10s /enter confirm/
Sleep 900ms
Down
Sleep 400ms
Up
Sleep 500ms
Enter
Wait+Screen@10s /Password/
Sleep 300ms
Type@40ms "demo-password-1"
Enter
Wait+Screen@20s /Happy building/
Sleep 3s

Hide
Type "clear && cd checkout"
Enter
Sleep 300ms
Show
Type "claude"
Enter
Wait+Screen@30s /auto mode on|for shortcuts/
Sleep 800ms
Type "/teamf project"
Sleep 400ms
Enter
Wait+Screen@90s /Which (project|one)|which (project|one)/
Sleep 2.5s
Type "/teamf project checkout"
Sleep 400ms
Enter
Wait+Screen@90s /(now works on|works on the|joined|shared)/
Sleep 3s
