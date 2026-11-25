#!/bin/sh
# Install teamflow from the teamflow-cli Releases page (macOS and Linux, amd64 and arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/vutran1710/teamflow-cli/main/install.sh | sh
#
# It finds the latest release's tag (from the Releases page's /latest redirect; no API), downloads that release's
# archive for this machine, checksums.txt and checksums.txt.minisig into a temporary folder; checks the signature
# of checksums.txt and that it was signed for this tag when minisign is installed (otherwise it says only the
# checksum was checked, and how to check the signature by hand); checks the archive's SHA-256 against
# checksums.txt; and only then copies the binary into ~/.local/bin (or $TEAMFLOW_BIN_DIR). It never runs anything
# it downloaded.
#
#   TEAMFLOW_BIN_DIR       where the binary goes (default ~/.local/bin)
#   TEAMFLOW_VERSION       a release tag such as v0.1.0 (default: the latest release)
#   TEAMFLOW_RELEASES_URL  the Releases page (default https://github.com/vutran1710/teamflow-cli/releases; for
#                          mirrors and tests: https, or http on localhost)
#
# The whole script is one function called on the last line, so a download cut short runs nothing.

main() {
	set -eu

	REPO="vutran1710/teamflow-cli"
	# The release key (minisign, ed25519): the same as internal/selfupdate/pubkey.go. Empty until the first release.
	PUBKEY=""

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

	releases="${TEAMFLOW_RELEASES_URL:-https://github.com/$REPO/releases}"
	https=1
	case "$releases" in
	*@*) fail "TEAMFLOW_RELEASES_URL must be an https URL" ;;
	https://?*) ;;
	http://127.0.0.1:* | http://localhost:*)
		# A local mirror or test server only: http://127.0.0.1:<port> or http://localhost:<port>[/path].
		port="${releases#http://*:}"
		port="${port%%/*}"
		case "$port" in
		"" | *[!0-9]*) fail "TEAMFLOW_RELEASES_URL must be an https URL" ;;
		esac
		https=0
		;;
	*) fail "TEAMFLOW_RELEASES_URL must be an https URL" ;;
	esac
	tag="${TEAMFLOW_VERSION:-}"
	if [ -n "$tag" ]; then
		is_tag "$tag" || fail "TEAMFLOW_VERSION must be a release tag such as v0.1.0"
	fi
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

	if [ -z "$tag" ]; then
		latest="$(get -o /dev/null -w '%{url_effective}' "$releases/latest" 2>/dev/null)" ||
			fail "cannot find the latest release at $releases (offline, or none published yet)"
		tag="${latest##*/tag/}"
		is_tag "$tag" || fail "cannot find the latest release at $releases (offline, or none published yet)"
	fi
	base="$releases/download/$tag"

	new=""
	tmp="$(mktemp -d "${TMPDIR:-/tmp}/teamflow-install.XXXXXX")"
	trap 'rm -rf "$tmp"; if [ -n "$new" ]; then rm -f "$new"; fi' EXIT
	trap 'exit 130' INT TERM

	echo "Downloading teamflow $tag ($asset) ..."
	fetch "$asset"
	fetch checksums.txt
	fetch checksums.txt.minisig

	# The signature of checksums.txt, and the tag it was signed for.
	if [ -z "$PUBKEY" ]; then
		echo "Note: this install.sh carries no release key yet; only the checksum was checked."
	elif command -v minisign >/dev/null 2>&1; then
		comment="$(cd "$tmp" && minisign -Vm checksums.txt -x checksums.txt.minisig -P "$PUBKEY" -Q 2>/dev/null)" ||
			fail "signature check failed for checksums.txt; refusing to install"
		check_signed_tag "$comment" "$tag"
		echo "The signature of checksums.txt is verified for $tag (signature verified)."
	else
		echo "Note: minisign is not installed, so only the checksum was checked; to verify the signature:"
		echo "  download checksums.txt and checksums.txt.minisig from $base, then run"
		echo "  minisign -Vm checksums.txt -x checksums.txt.minisig -P $PUBKEY"
		echo "  (its trusted comment must name tag:$tag)"
	fi

	# The archive's checksum.
	want="$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")"
	[ -n "$want" ] || fail "checksums.txt has no checksum for $asset; refusing to install"
	got="$(sha256 "$tmp/$asset")"
	[ "$got" = "$want" ] || fail "checksum mismatch for $asset (a damaged or changed download); refusing to install"

	# Unpack (verified by now) and copy into place: a temporary file in the folder, then a rename.
	mkdir "$tmp/x"
	tar -xzf "$tmp/$asset" -C "$tmp/x" || fail "$asset could not be unpacked"
	[ -f "$tmp/x/teamflow" ] || fail "$asset holds no teamflow binary; refusing to install"
	mkdir -p "$bindir" 2>/dev/null || fail "cannot write to $bindir; set TEAMFLOW_BIN_DIR to a folder you can write"
	new="$(mktemp "$bindir/.teamflow-install.XXXXXX" 2>/dev/null)" ||
		fail "cannot write to $bindir; set TEAMFLOW_BIN_DIR to a folder you can write"
	if ! cp "$tmp/x/teamflow" "$new" 2>/dev/null || ! chmod 0755 "$new" || ! mv -f "$new" "$bindir/teamflow"; then
		fail "cannot write to $bindir; set TEAMFLOW_BIN_DIR to a folder you can write"
	fi

	new=""
	echo "Installed teamflow $tag to $bindir/teamflow"
	found="$(command -v teamflow 2>/dev/null || true)"
	case ":$PATH:" in
	*":$bindir:"*)
		if [ -n "$found" ] && [ "$found" != "$bindir/teamflow" ]; then
			path_warning "the teamflow on your PATH is $found, not this one ($bindir/teamflow)"
		fi
		;;
	*) path_warning "teamflow is not on your PATH ($bindir is not in it)" ;;
	esac
	echo
	echo "Next: teamflow install -a <agent> -e <email> [-s <server>]"
	echo "  (agents: claude-code, cursor, antigravity, copilot; the server defaults to https://teamflow.vutr.io)"
}

# path_warning says loudly that `teamflow` run by name is not the binary just installed, and how to fix it for zsh
# and bash ($HOME for the home folder). The agents' hooks call it by its full path; the rest runs it by name.
path_warning() {
	shown="$bindir"
	case "$bindir" in
	"$HOME"/*) shown="\$HOME/${bindir#"$HOME"/}" ;;
	esac
	echo
	echo "WARNING: $1."
	echo "Add its folder to your PATH, then open a new terminal:"
	echo "  zsh:  echo 'export PATH=\"$shown:\$PATH\"' >> ~/.zshrc"
	echo "  bash: echo 'export PATH=\"$shown:\$PATH\"' >> ~/.bashrc"
	echo "Your agents' hooks call teamflow by its full path and work as they are; you, the /teamf command and the"
	echo "agents' own \`teamflow ...\` commands run it by name and need it on the PATH."
}

fail() {
	echo "teamflow install: $*" >&2
	exit 1
}

# is_tag: a release tag vX.Y.Z.
is_tag() {
	case "$1" in
	v[0-9]*.[0-9]*.[0-9]*)
		case "$1" in
		*[!0-9v.]* | v*v* | *..* | *.) return 1 ;;
		esac
		return 0
		;;
	esac
	return 1
}

# get is curl; for an https Releases page, every hop (redirects too) must stay https.
get() {
	if [ "$https" = 1 ]; then
		curl --proto '=https' --proto-redir '=https' -fsSL "$@"
	else
		curl -fsSL "$@"
	fi
}

fetch() {
	get --retry 2 -o "$tmp/$1" "$base/$1" || fail "download failed: $base/$1"
}

# check_signed_tag COMMENT TAG: the trusted comment's one tab-separated "tag:" field is TAG.
check_signed_tag() {
	found="$(printf '%s\n' "$1" | awk -F '\t' '{for (i = 1; i <= NF; i++) if (substr($i, 1, 4) == "tag:") {n++; t = substr($i, 5)}} END {print n + 0 " " t}')"
	case "${found%% *}" in
	0) fail "the signature names no release tag; refusing to install" ;;
	1) ;;
	*) fail "the signature names more than one release tag; refusing to install" ;;
	esac
	[ "${found#* }" = "$2" ] || fail "the signature was signed for ${found#* }, not $2; refusing to install"
}

main "$@"
