# The README's clip: install, set up Claude Code, see your projects. Re-record with charmbracelet VHS:
#
#   TEAMFLOW_SERVER_CLI=/path/to/teamflow-server sh docs/demo-setup.sh "$DIR"   # a local server, a demo account
#   ln -sfn "$DIR/home" /tmp/you                                                # the clip's (throwaway) home
#   vhs docs/demo.tape
#   HOME=/tmp/you teamflow sync --stop; kill "$(cat "$DIR/server.pid")"; rm -rf "$DIR" /tmp/you
#
# Everything happens under /tmp/you: the clip never reads or writes your own home, agents or Teamflow sign-in.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 15
Set Width 1100
Set Height 560
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 18ms
Set PlaybackSpeed 2.2

Hide
Type "export HOME=/tmp/you PATH=/tmp/you/.local/bin:/usr/bin:/bin PS1='$ ' && cd ~ && clear"
Enter
Show

Type "curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh"
Enter
Wait+Screen@20s /Next: teamflow install/
Sleep 1.2s

Type "teamflow install -a claude-code -e you@example.com"
Enter
Wait+Screen@10s /Password/
Sleep 300ms
Type "demo-password-1"
Enter
Wait+Screen@20s /Done\./
Sleep 1.5s

Type "teamflow projects"
Enter
Wait+Screen@10s /checkout/
Sleep 2.5s
