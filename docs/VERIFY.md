# Verifying a teamflow release

Each release on the [Releases page](https://github.com/vutran1710/teamflow-cli/releases) carries:

- `teamflow_<os>_<arch>.tar.gz` for `darwin` and `linux` on `amd64` and `arm64` (the binary, `LICENSE`, `README.md`);
- `checksums.txt`: the SHA-256 of each archive;
- `checksums.txt.minisig`: a [minisign](https://jedisct1.github.io/minisign/) signature (ed25519) of `checksums.txt`.
  Its trusted comment (signed too) is `timestamp:<unix>	file:checksums.txt	tag:<the release's tag>`.

The release key's public half is compiled into `teamflow` (`internal/selfupdate/pubkey.go`) and written in
`install.sh` (its `PUBKEY` line); the two are kept equal by a test. Its secret half lives only in the secret
`TEAMFLOW_SIGNING_KEY` of this repo's GitHub environment `release`; it is never committed or printed.

## Who checks what

- **`teamflow self-update`** (and `/teamf upgrade`, and the background sync's scheduled update, which run the same
  code) always checks the signature (with the compiled-in key, using Go's standard library) and that it was made
  for the release's tag, then the archive's checksum, before it touches the installed binary. It then checks the
  new binary runs and names the release's version, and renames it over the old one in the same folder. Downloads
  and their redirects must be https. On any failure it refuses and the old binary stays as it was. The background
  sync swaps the binary only while no send is in flight (it holds every outbox's sender lock), then restarts on it.
- **`install.sh`** finds the latest release's tag from the Releases page's `/latest` redirect (no API) and
  downloads that tag's files. It always checks the checksum. When `minisign` is installed it checks the signature
  and that it was made for that tag; otherwise it says that only the checksum was checked and prints the command
  below. Nothing it downloaded runs before both checks pass; then, at a terminal, it offers to run
  `teamflow install`.

A checksum alone shows the download is intact, not who made it: the checksum file comes from the same place as
the archive. The signature shows the checksum file was made with the release key; its tag shows for which release
(so an older release's signed files cannot be passed off as the latest).

## By hand

```sh
minisign -Vm checksums.txt -x checksums.txt.minisig -P <the PUBKEY line of install.sh>   # shows tag:<release>
sha256sum -c --ignore-missing checksums.txt      # macOS: shasum -a 256 -c --ignore-missing checksums.txt
```

The signature is minisign's legacy (pure ed25519) form, which `minisign -V` accepts; it is what Go's standard
library can check, so self-update needs nothing else.

## Choices

- **A binary reached through a symlink** (for example `~/.local/bin/teamflow -> /opt/teamflow/teamflow`):
  `self-update` replaces the file the link points to, and leaves the link alone. A link that points nowhere is
  refused.
- **A folder that cannot be written** (a read-only or system location): `self-update` refuses before it downloads
  anything, and says to reinstall with `install.sh` (or run it as a user who can write there).
- **Two updates at once** (`self-update`, `/teamf upgrade`, a background sync's, from any Teamflow home): they take
  turns on `.teamflow-update.lock` beside the binary (it stays there), and an update replaces the binary only if
  the path still names the file it started from; otherwise it changes nothing.
- **No build for this machine** (another OS or architecture): refused, nothing changed. `install.sh` refuses an
  unknown OS or architecture before it downloads anything.
- **GitHub's API rate limit or no network:** `self-update` refuses with the reason and the Releases link.
  `install.sh` does not use the API.
- **Up to date, or a newer build than the latest release:** nothing is downloaded. A pre-release, or a tag that
  is not a release version, is never installed. A build that is not a release version (`dev`, a commit build) is
  updated to the latest release by `self-update`, never by the background sync.

## The release gate and the key (once, by the maintainer)

The key is a secret of the GitHub **environment** `release`, not of the repo: only jobs that declare that
environment (the release workflow's `sign` and `publish`) can read it, and the environment takes only `v*` tags. A
tag ruleset lets only admins create, move or delete `v*` tags. Pushing a `v*` tag is the release: there is no
manual approval step.

Run steps 1–4 in one shell session: they share `$REPO` and `$d` (the key's temporary folder).

1. The environment (only `v*` tags deploy to it; no required reviewer):

   ```sh
   REPO=vutran1710/teamflow-cli
   printf '%s' '{"wait_timer": 0,
     "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}' |
     gh api -X PUT "repos/$REPO/environments/release" --input -
   gh api -X POST "repos/$REPO/environments/release/deployment-branch-policies" -f name='v*' -f type=tag
   ```

2. Only admins may create, move or delete `v*` tags (repository role 5 is Admin):

   ```sh
   printf '%s' '{"name": "release tags", "target": "tag", "enforcement": "active",
     "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
     "rules": [{"type": "creation"}, {"type": "update"}, {"type": "deletion"}],
     "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}]}' |
     gh api -X POST "repos/$REPO/rulesets" --input -
   ```

3. The key, made in a private temporary folder (umask 077), never inside a repo, and stored as the environment's
   secret:

   ```sh
   d="$(mktemp -d)" && (umask 077 && cd "$d" && minisign -G -W -p teamflow-release.pub -s teamflow-release.key)
   gh secret set TEAMFLOW_SIGNING_KEY --env release --repo "$REPO" < "$d/teamflow-release.key"
   gh secret list --env release --repo "$REPO"   # TEAMFLOW_SIGNING_KEY is listed here
   gh secret list --repo "$REPO"                 # and not as a repo secret (gh secret delete it if it is)
   tail -1 "$d/teamflow-release.pub"             # the one-line public key
   ```

   `-W` makes an unencrypted key: it lives only in the environment secret and the offline backup.

4. **Back the secret key up offline** (a password manager entry or an encrypted drive). Losing it strands every
   installed client: their `self-update` refuses every later release until people reinstall with `install.sh`.
   Then delete the files: `rm -rf "$d"`.

5. Put the one-line public key in `internal/selfupdate/pubkey.go` (`const PublicKey = "…"`, dropping the
   PLACEHOLDER note) and in `install.sh` (`PUBKEY="…"`); `go test ./...` checks the two are equal and valid. Commit,
   push to main, and wait for green CI. A tag is pushed only after that, so the release's binaries carry the key.

Until the public key is committed, `self-update` refuses ("this build has no release key") and the release workflow
refuses to sign.

## The release workflow

Four jobs, each with the least it needs: `build` (read-only, no secret: tests and GoReleaser, which never
publishes), `sign` (read-only, environment `release`, the only job that names the secret; it runs only this repo's
code, after checking the archives against `checksums.txt`), `verify` (read-only, no secret: the stock minisign,
the signed tag and the binary's version) and `publish` (environment `release`, the only job that can write, no
secret: `tools/signsums -verify`, then `gh release create`). Every action in every workflow is pinned to a commit
SHA, and every workflow starts from `permissions: {}`; `tools/workflows` tests these, that only `sign` names the
key, and that `sign` and `publish` declare the `release` environment.

Signing is done by `tools/signsums` (standard library only, no minisign on the runner). It refuses an empty
secret, an encrypted key, a key that is not the committed one, a tag that is not `vX.Y.Z`, and a signature that
does not verify, and it writes no signature file then.
