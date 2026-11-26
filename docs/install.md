# Installing teamflow

macOS and Linux (amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh
```

At a terminal, once the binary is verified and in place, the installer asks "Set up teamflow now?" and runs
`teamflow install`, which asks for your email, the agents to set up (those found on this machine come chosen) and
your password. `TEAMFLOW_NO_SETUP=1` (or CI, or no terminal) skips the question; run `teamflow install` later, or
with flags in a script: `teamflow install -e <email> -a <agent> [-s <server>] --password-stdin`.

`install.sh` puts `teamflow` in `~/.local/bin` (or `$TEAMFLOW_BIN_DIR`; `TEAMFLOW_VERSION=v0.1.0` pins a release).
It checks the archive's SHA-256 against the release's `checksums.txt`, and that file's minisign (ed25519) signature
when `minisign` is installed; otherwise it says only the checksum was checked and how to check the signature by
hand. You can also download a release from this repo's Releases page directly. There is no Homebrew formula.

**`teamflow` must be on your PATH.** Your agents' hooks call the binary by its full path, so they work wherever it
is; but you, the `/teamf` command and the agents themselves run `teamflow ...` by name (the instruction block can
only tell them the command, not where it lives). When the folder is not on the PATH, or another `teamflow` (an
older install, the retired Python one) comes first, `install.sh` and `teamflow install` still install but say
so, with the line to add for zsh (`~/.zshrc`) and bash (`~/.bashrc`); `teamflow install --json` says
`"on_path": false`.

`teamflow self-update` replaces the binary with the newest release, always checking the signature (with the key
compiled into the binary) and the checksum first; on any failure the old binary stays. See
[VERIFY.md](VERIFY.md).

To build from source (Go 1.26 or later):

```sh
go install github.com/vutran1710/teamflow-cli/cmd/teamflow@latest
teamflow --version
```
