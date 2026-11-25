# Verifying a teamflow release

Each release on the [Releases page](https://github.com/vutran1710/teamflow-cli/releases) carries:

- `teamflow_<os>_<arch>.tar.gz` for `darwin` and `linux` on `amd64` and `arm64` (the binary, `LICENSE`, `README.md`);
- `checksums.txt`: the SHA-256 of each archive;
- `checksums.txt.minisig`: a [minisign](https://jedisct1.github.io/minisign/) signature (ed25519) of `checksums.txt`.

The release key's public half is compiled into `teamflow` (`internal/selfupdate/pubkey.go`) and written in
`install.sh` (its `PUBKEY` line); the two are kept equal by a test. Its secret half lives only in this repo's
GitHub Actions secret `TEAMFLOW_SIGNING_KEY`; it is never committed or printed.

## Who checks what

- **`teamflow self-update`** always checks the signature (with the compiled-in key, using Go's standard library)
  and then the archive's checksum, before it touches the installed binary. It then checks the new binary runs and
  names the release's version, and renames it over the old one in the same folder. On any failure it refuses and
  the old binary stays as it was.
- **`install.sh`** always checks the checksum. It checks the signature when `minisign` is installed; otherwise it
  says that only the checksum was checked and prints the command below. It never runs what it downloaded.

A checksum alone shows the download is intact, not who made it: the checksum file comes from the same place as
the archive. The signature shows the checksum file was made with the release key.

## By hand

```sh
minisign -Vm checksums.txt -x checksums.txt.minisig -P <the PUBKEY line of install.sh>
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
- **No build for this machine** (another OS or architecture): refused, nothing changed. `install.sh` refuses an
  unknown OS or architecture before it downloads anything.
- **GitHub's API rate limit or no network:** `self-update` refuses with the reason and the Releases link.
  `install.sh` does not use the API (it downloads from `releases/latest/download/`).
- **Up to date, or a newer build than the latest release:** nothing is downloaded. A build that is not a release
  version (`dev`, a commit build) is updated to the latest release.

## Making the release key (once, by the maintainer)

```sh
minisign -G -W -p teamflow-release.pub -s teamflow-release.key   # -W: unencrypted; it lives only in the secret
gh secret set TEAMFLOW_SIGNING_KEY --repo vutran1710/teamflow-cli < teamflow-release.key
tail -1 teamflow-release.pub    # the one-line key: put it in pubkey.go's PublicKey and install.sh's PUBKEY
```

Then keep the secret key offline (or delete it: a lost key means a new key and a release that says so). Until the
public key is committed, `self-update` refuses ("this build has no release key") and the release workflow refuses
to sign. A tag is pushed only after the key is committed, so the release's binaries carry it.

Signing is done by `tools/signsums` in the release workflow (standard library only, no minisign on the runner): it
refuses an empty secret, an encrypted key, a key that is not the committed one, and a signature that does not
verify; the workflow then checks the signature with the stock minisign before publishing.
