# The README's clip: the one curl line, then teamflow's setup (email, agent, password), then Claude Code choosing the
# project and showing where it stands. Recorded with charmbracelet VHS (and minisign installed), against the real
# Teamflow (https://teamflow.10l.ink) and the real release from GitHub, as a demo account (org Demo, project checkout):
#
#   DEMO_PASSWORD_FILE=/path/to/password sh docs/demo-setup.sh
#   vhs docs/demo.tape
#   sh docs/demo-setup.sh --clean
#
# The clip's home is /tmp/you (demo-setup.sh makes it): the clip never reads or writes your own home, agents or
# Teamflow sign-in. The password is typed at its prompt (echo off) by the home's .demo-type-password, from the file;
# it is never in this tape or the clip. Claude Code is the real one, signed in through your keychain, on a cheap model.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 22
Set Width 1150
Set Height 1000
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 25ms
Set PlaybackSpeed 1.3

Hide
Type "env -i HOME=/tmp/you USER=$USER TERM=xterm-256color LANG=en_US.UTF-8 PATH=/tmp/you/.local/bin:/tmp/you/.demo-bin:/usr/bin:/bin bash --norc --noprofile"
Enter
Type "export PS1='$ ' CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"
Enter
Type `claude() { command claude --model haiku "$@"; } && cd ~/checkout && claude`
Enter
Sleep 8s
Ctrl+C
Ctrl+C
Sleep 1s
Type "( ~/.demo-type-password >/dev/null 2>&1 & ) && cd ~ && clear"
Enter
Sleep 500ms
Show

Type "curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh"
Sleep 300ms
Enter
Wait+Screen@60s /Set up teamflow now/
Sleep 700ms
Type "y"
Enter
Wait+Screen@10s /Email:/
Sleep 300ms
Type "demo@teamflow.dev"
Sleep 300ms
Enter
Wait+Screen@10s /enter confirm/
Sleep 900ms
Down
Sleep 400ms
Up
Sleep 500ms
Enter
Wait+Screen@20s /Happy building/
Sleep 3.5s

Hide
Type "clear && cd checkout"
Enter
Sleep 300ms
Show
Type "claude"
Enter
Wait+Screen@30s /auto mode on|for shortcuts/
Sleep 800ms
Type "/teamf project checkout"
Sleep 400ms
Enter
Sleep 1.5s
Hide
Wait+Screen@90s /(now works on|works on the|shared with)/
Sleep 1s
Show
Sleep 2.5s
Type "/teamf status"
Sleep 400ms
Enter
Sleep 1.5s
Hide
Wait+Screen@90s /Open drifts/
Sleep 1.5s
Show
Sleep 6s
