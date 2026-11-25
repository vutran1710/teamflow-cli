#!/bin/sh
# Install teamflow from the teamflow-cli Releases page (macOS and Linux, amd64 and arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh
#
# It downloads the archive for this machine, checksums.txt and checksums.txt.minisig into a temporary folder;
# checks the signature of checksums.txt when minisign is installed (otherwise it says only the checksum was
# checked, and how to check the signature by hand); checks the archive's SHA-256 against checksums.txt; and only
# then copies the binary into ~/.local/bin (or $TEAMFLOW_BIN_DIR). It never runs anything it downloaded.
#
#   TEAMFLOW_BIN_DIR       where the binary goes (default ~/.local/bin)
#   TEAMFLOW_VERSION       a release tag such as v0.1.0 (default: the latest release)
#   TEAMFLOW_DOWNLOAD_URL  where the release files are (default: the Releases page; for mirrors and tests)
set -eu

REPO="vutran1710/teamflow-cli"
# The release key (minisign, ed25519): the same as internal/selfupdate/pubkey.go. Empty until the first release.
PUBKEY=""

fail() {
	echo "teamflow install: $*" >&2
	exit 1
}

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "this system ($(uname -s)) is not supported; teamflow runs on macOS and Linux" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "this architecture ($(uname -m)) is not supported; teamflow runs on amd64 and arm64" ;;
esac
asset="teamflow_${os}_${arch}.tar.gz"

version="${TEAMFLOW_VERSION:-}"
case "$version" in
"") base="https://github.com/$REPO/releases/latest/download" ;;
v[0-9]*.[0-9]*.[0-9]*)
	case "$version" in
	*[!0-9A-Za-z.-]*) fail "TEAMFLOW_VERSION must be a release tag such as v0.1.0" ;;
	esac
	base="https://github.com/$REPO/releases/download/$version"
	;;
*) fail "TEAMFLOW_VERSION must be a release tag such as v0.1.0" ;;
esac
base="${TEAMFLOW_DOWNLOAD_URL:-$base}"
bindir="${TEAMFLOW_BIN_DIR:-$HOME/.local/bin}"

command -v curl >/dev/null 2>&1 || fail "curl is needed to download teamflow"
command -v tar >/dev/null 2>&1 || fail "tar is needed to unpack teamflow"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	fail "sha256sum or shasum is needed to check the download"
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/teamflow-install.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT INT TERM

fetch() {
	curl -fsSL --retry 2 -o "$tmp/$1" "$base/$1" || fail "download failed: $base/$1"
}

echo "Downloading $asset ..."
fetch "$asset"
fetch checksums.txt
fetch checksums.txt.minisig

# The signature of checksums.txt.
verify_by_hand="minisign -Vm checksums.txt -x checksums.txt.minisig -P $PUBKEY"
if [ -z "$PUBKEY" ]; then
	echo "Note: this install.sh carries no release key yet; only the checksum was checked."
elif command -v minisign >/dev/null 2>&1; then
	(cd "$tmp" && minisign -Vm checksums.txt -x checksums.txt.minisig -P "$PUBKEY" >/dev/null 2>&1) ||
		fail "signature check failed for checksums.txt; refusing to install"
	echo "The signature of checksums.txt is verified (signature verified)."
else
	echo "Note: minisign is not installed, so only the checksum was checked; to verify the signature:"
	echo "  download checksums.txt and checksums.txt.minisig from $base, then run"
	echo "  $verify_by_hand"
fi

# The archive's checksum.
want="$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "checksums.txt has no checksum for $asset; refusing to install"
got="$(sha256 "$tmp/$asset")"
[ "$got" = "$want" ] || fail "checksum mismatch for $asset (a damaged or changed download); refusing to install"

# Unpack (verified by now) and copy into place: a temporary name in the folder, then a rename.
mkdir "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x" || fail "$asset could not be unpacked"
[ -f "$tmp/x/teamflow" ] || fail "$asset holds no teamflow binary; refusing to install"
mkdir -p "$bindir" 2>/dev/null || fail "cannot write to $bindir; set TEAMFLOW_BIN_DIR to a folder you can write"
new="$bindir/.teamflow-install.$$"
cp "$tmp/x/teamflow" "$new" 2>/dev/null || {
	rm -f "$new" 2>/dev/null
	fail "cannot write to $bindir; set TEAMFLOW_BIN_DIR to a folder you can write"
}
chmod 0755 "$new"
mv -f "$new" "$bindir/teamflow" || {
	rm -f "$new"
	fail "cannot write to $bindir; set TEAMFLOW_BIN_DIR to a folder you can write"
}

echo "Installed teamflow to $bindir/teamflow"
case ":$PATH:" in
*":$bindir:"*) ;;
*) echo "Note: $bindir is not on your PATH; add it, for example: export PATH=\"$bindir:\$PATH\"" ;;
esac
echo
echo "Next: teamflow install -a <agent> -e <email> [-s <server>]"
echo "  (agents: claude-code, cursor, antigravity, copilot; the server defaults to https://teamflow.vutr.io)"
