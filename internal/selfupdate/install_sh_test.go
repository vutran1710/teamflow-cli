package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/minisign"
)

// install.sh runs under sh with a temp HOME, a PATH holding only the tools it needs (plus a fake uname and, when
// a test wants one, a fake or the real minisign), and a local server standing in for the Releases page.

// The downloaded binary leaves a mark if it is ever run: install.sh never runs what it downloads.
const markScript = "#!/bin/sh\ntouch \"$MARK\"\necho \"teamflow 0.2.0\"\n"

// shRelease stands in for https://github.com/<repo>/releases: /latest redirects to /tag/<tag>, and the files are
// under /download/<tag>/.
type shRelease struct {
	srv      *httptest.Server
	tag      string
	files    map[string][]byte
	noLatest bool
	mu       sync.Mutex
	requests int
}

func newShRelease(t *testing.T, sk minisign.SecretKey, osName, arch string) *shRelease {
	t.Helper()
	r := &shRelease{tag: "v0.2.0", files: map[string][]byte{}}
	name := AssetName(osName, arch)
	r.files[name] = tarGz(t, map[string]string{"teamflow": markScript, "LICENSE": "MIT\n"})
	h := sha256.Sum256(r.files[name])
	sums := []byte(hex.EncodeToString(h[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  teamflow_other.tar.gz\n")
	r.files["checksums.txt"] = sums
	r.files["checksums.txt.minisig"] = minisign.Sign(sk, sums, signed(r.tag))
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests++
		noLatest := r.noLatest
		b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/download/"+r.tag+"/")]
		r.mu.Unlock()
		switch {
		case req.URL.Path == "/latest" && !noLatest:
			http.Redirect(w, req, "/tag/"+r.tag, http.StatusFound)
		case req.URL.Path == "/tag/"+r.tag:
			_, _ = w.Write([]byte("<html>release page</html>"))
		case ok && strings.HasPrefix(req.URL.Path, "/download/"+r.tag+"/"):
			_, _ = w.Write(b)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

type shRun struct {
	t       *testing.T
	script  string
	home    string
	tools   string
	env     []string
	out     string
	code    int
	mark    string
	release *shRelease
}

// shPlatform is what the fake uname says for this machine.
func shPlatform() (string, string) {
	osName := map[string]string{"darwin": "Darwin", "linux": "Linux"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	return osName, arch
}

// newShRun copies install.sh with pub as its PUBKEY (a throwaway key; "" keeps the placeholder) into a temp folder.
func newShRun(t *testing.T, pub string, rel *shRelease) *shRun {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install.sh is for macOS and Linux")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(src), "\n\tPUBKEY=\""+PublicKey+"\"\n", "\n\tPUBKEY=\""+pub+"\"\n", 1)
	root := t.TempDir()
	s := &shRun{t: t, script: filepath.Join(root, "install.sh"), home: filepath.Join(root, "home"), tools: filepath.Join(root, "tools"), mark: filepath.Join(root, "ran"), release: rel}
	if err := os.WriteFile(s.script, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{s.home, s.tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only the tools install.sh may use, from the system folders (never a minisign found there).
	for _, tool := range []string{"sh", "curl", "tar", "gzip", "awk", "mktemp", "sha256sum", "shasum", "chmod", "mkdir", "mv", "cp", "rm", "cat", "sed", "tr", "head", "dirname", "perl"} {
		for _, dir := range []string{"/usr/bin", "/bin"} {
			p := filepath.Join(dir, tool)
			if _, err := os.Stat(p); err == nil {
				if err := os.Symlink(p, filepath.Join(s.tools, tool)); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
	}
	osName, arch := shPlatform()
	s.fake("uname", "#!/bin/sh\ncase \"$1\" in -s) echo \"${FAKE_OS:-"+osName+"}\";; -m) echo \"${FAKE_ARCH:-"+arch+"}\";; *) echo \"${FAKE_OS:-"+osName+"}\";; esac\n")
	return s
}

func (s *shRun) fake(name, script string) {
	_ = os.Remove(filepath.Join(s.tools, name)) // a link to the system's tool is replaced, never written through
	if err := os.WriteFile(filepath.Join(s.tools, name), []byte(script), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *shRun) run(extra ...string) {
	s.t.Helper()
	cmd := exec.Command(filepath.Join(s.tools, "sh"), s.script)
	cmd.Env = append([]string{"HOME=" + s.home, "PATH=" + s.tools, "MARK=" + s.mark, "TEAMFLOW_RELEASES_URL=" + s.release.srv.URL}, extra...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	s.out, s.code = buf.String(), 0
	if ee, ok := err.(*exec.ExitError); ok {
		s.code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	if _, err := os.Stat(s.mark); err == nil {
		s.t.Fatalf("install.sh ran the downloaded binary\n%s", s.out)
	}
}

func (s *shRun) installed(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "teamflow"))
	return err == nil && string(b) == markScript
}

func (s *shRun) wantOK() {
	s.t.Helper()
	if s.code != 0 {
		s.t.Fatalf("exit %d\n%s", s.code, s.out)
	}
}

func (s *shRun) wantRefused(text string) {
	s.t.Helper()
	if s.code == 0 {
		s.t.Fatalf("not refused\n%s", s.out)
	}
	if !strings.Contains(s.out, text) {
		s.t.Fatalf("refusal does not say %q\n%s", text, s.out)
	}
	if s.installed(filepath.Join(s.home, ".local", "bin")) {
		s.t.Fatalf("installed anyway\n%s", s.out)
	}
}

func platformAsset() string { return AssetName(runtime.GOOS, runtime.GOARCH) }

// Happy path without minisign: installs to ~/.local/bin (0755), says only the checksum was checked and how to check
// the signature by hand, says the next step, and never runs the binary. The temp folder is gone afterwards.
func TestInstallShNoMinisignNote(t *testing.T) {
	sk, pub := throwawayKey(t)
	s := newShRun(t, pub, newShRelease(t, sk, runtime.GOOS, runtime.GOARCH))
	s.run()
	s.wantOK()
	bin := filepath.Join(s.home, ".local", "bin")
	if !s.installed(bin) {
		t.Fatalf("not installed\n%s", s.out)
	}
	if fi, _ := os.Stat(filepath.Join(bin, "teamflow")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	for _, want := range []string{
		"only the checksum was checked; to verify the signature:",
		"minisign -Vm checksums.txt -x checksums.txt.minisig -P " + pub,
		"teamflow install -a <agent> -e <email> [-s <server>]",
		"is not on your PATH", // the temp HOME's bin is not on the test's PATH
	} {
		if !strings.Contains(s.out, want) {
			t.Fatalf("output lacks %q\n%s", want, s.out)
		}
	}
}

// With minisign: a good signature is verified with the embedded key (and no note); a bad one is refused before
// anything is installed; a missing signature file is refused with or without minisign.
func TestInstallShMinisign(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	s := newShRun(t, pub, rel)
	args := filepath.Join(t.TempDir(), "args")
	s.fake("minisign", fakeMinisign(args, signed("v0.2.0")))
	s.run()
	s.wantOK()
	if b, _ := os.ReadFile(args); strings.TrimSpace(string(b)) != "-Vm checksums.txt -x checksums.txt.minisig -P "+pub+" -Q" {
		t.Fatalf("minisign args %q", b)
	}
	if strings.Contains(s.out, "only the checksum") || !strings.Contains(s.out, "signature verified") {
		t.Fatalf("output:\n%s", s.out)
	}

	s = newShRun(t, pub, rel)
	s.fake("minisign", "#!/bin/sh\nexit 1\n")
	s.run()
	s.wantRefused("signature check failed")

	// The signature is good but made for another release (an older one served as the latest), or names no tag.
	for comment, want := range map[string]string{
		signed("v0.1.0"):                  "signed for v0.1.0, not v0.2.0",
		"timestamp:1\tfile:checksums.txt": "names no release tag",
		signed("v0.2.0") + "\ttag:v0.1.0": "names more than one release tag",
	} {
		s = newShRun(t, pub, rel)
		s.fake("minisign", fakeMinisign(args, comment))
		s.run()
		s.wantRefused(want)
	}

	delete(rel.files, "checksums.txt.minisig")
	s = newShRun(t, pub, rel)
	s.run()
	s.wantRefused("checksums.txt.minisig")
	s = newShRun(t, pub, rel)
	s.fake("minisign", "#!/bin/sh\nexit 0\n")
	s.run()
	s.wantRefused("checksums.txt.minisig")
}

// fakeMinisign records its arguments and, like `minisign -V -Q`, prints the trusted comment.
func fakeMinisign(args, comment string) string {
	return "#!/bin/sh\necho \"$@\" > " + args + "\nprintf '%s\\n' '" + comment + "'\n"
}

// The latest release is found from the /latest redirect (no API); the files come from that tag's folder. A
// pinned TEAMFLOW_VERSION does not ask for the latest; no latest release is refused.
func TestInstallShFindsTheTag(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	s := newShRun(t, pub, rel)
	s.run()
	s.wantOK()
	if !strings.Contains(s.out, "teamflow v0.2.0") {
		t.Fatalf("output does not name the release:\n%s", s.out)
	}
	rel.noLatest = true
	s = newShRun(t, pub, rel)
	s.run()
	s.wantRefused("cannot find the latest release")
	s = newShRun(t, pub, rel)
	s.run("TEAMFLOW_VERSION=v0.2.0")
	s.wantOK()
	s = newShRun(t, pub, rel)
	s.run("TEAMFLOW_VERSION=v0.1.0")
	s.wantRefused("download failed")
}

// The real minisign tool (when installed, as in CI) checks a signature made by package minisign; a changed
// checksums.txt fails it.
func TestInstallShRealMinisign(t *testing.T) {
	real, err := exec.LookPath("minisign")
	if err != nil {
		t.Skip("minisign is not installed")
	}
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	s := newShRun(t, pub, rel)
	if err := os.Symlink(real, filepath.Join(s.tools, "minisign")); err != nil {
		t.Fatal(err)
	}
	s.run()
	s.wantOK()
	if !strings.Contains(s.out, "signature verified") {
		t.Fatalf("output:\n%s", s.out)
	}
	other, _ := throwawayKey(t)
	rel.files["checksums.txt.minisig"] = minisign.Sign(other, rel.files["checksums.txt"], signed("v0.2.0"))
	s = newShRun(t, pub, rel)
	if err := os.Symlink(real, filepath.Join(s.tools, "minisign")); err != nil {
		t.Fatal(err)
	}
	s.run()
	s.wantRefused("signature check failed")
	// The real tool's -Q output is read for the tag: a good signature for an older release is refused.
	rel.files["checksums.txt.minisig"] = minisign.Sign(sk, rel.files["checksums.txt"], signed("v0.1.0"))
	s = newShRun(t, pub, rel)
	if err := os.Symlink(real, filepath.Join(s.tools, "minisign")); err != nil {
		t.Fatal(err)
	}
	s.run()
	s.wantRefused("signed for v0.1.0, not v0.2.0")
}

// Sad paths: the archive does not match checksums.txt; checksums.txt has no line for this build; the archive is
// missing (404); the archive holds no binary. Nothing is installed or run.
func TestInstallShRefusesBadChecksum(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	good := rel.files[platformAsset()]
	rel.files[platformAsset()] = tarGz(t, map[string]string{"teamflow": markScript + "# changed\n"})
	s := newShRun(t, pub, rel)
	s.run()
	s.wantRefused("checksum mismatch")

	rel.files[platformAsset()] = good
	rel.files["checksums.txt"] = []byte(strings.Repeat("0", 64) + "  teamflow_other.tar.gz\n")
	s = newShRun(t, pub, rel)
	s.run()
	s.wantRefused("no checksum for " + platformAsset())

	delete(rel.files, platformAsset())
	s = newShRun(t, pub, rel)
	s.run()
	s.wantRefused("download failed")

	empty := tarGz(t, map[string]string{"README": "x"})
	h := sha256.Sum256(empty)
	rel.files[platformAsset()] = empty
	rel.files["checksums.txt"] = []byte(hex.EncodeToString(h[:]) + "  " + platformAsset() + "\n")
	s = newShRun(t, pub, rel)
	s.run()
	s.wantRefused("holds no teamflow binary")
}

// Sad paths: an unknown architecture or OS is refused before anything is downloaded; so is a malformed
// TEAMFLOW_VERSION.
func TestInstallShRefusesUnknownPlatform(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	for _, env := range [][]string{
		{"FAKE_ARCH=riscv64"}, {"FAKE_ARCH=i686"}, {"FAKE_OS=MINGW64_NT-10.0"}, {"FAKE_OS=FreeBSD"},
	} {
		s := newShRun(t, pub, rel)
		s.run(env...)
		s.wantRefused("not supported")
	}
	s := newShRun(t, pub, rel)
	s.run("TEAMFLOW_VERSION=v1;rm")
	s.wantRefused("TEAMFLOW_VERSION")
	s = newShRun(t, pub, rel)
	s.run("TEAMFLOW_RELEASES_URL=ftp://example.invalid")
	s.wantRefused("TEAMFLOW_RELEASES_URL")
	if rel.requests != 0 {
		t.Fatalf("downloaded %d files", rel.requests)
	}
}

// TEAMFLOW_BIN_DIR: installs there (made if missing), replaces an older binary; a folder that cannot be written is
// refused after the checks, leaving its binary as it was.
func TestInstallShBinDir(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	dir := filepath.Join(t.TempDir(), "my", "bin")
	s := newShRun(t, pub, rel)
	s.run("TEAMFLOW_BIN_DIR=" + dir)
	s.wantOK()
	if !s.installed(dir) {
		t.Fatalf("not installed in %s\n%s", dir, s.out)
	}
	if err := os.WriteFile(filepath.Join(dir, "teamflow"), []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	s.run("TEAMFLOW_BIN_DIR=" + dir)
	s.wantOK()
	if !s.installed(dir) {
		t.Fatal("the older binary was not replaced")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind %v", entries)
	}

	if os.Geteuid() == 0 {
		return
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "teamflow"), []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	s = newShRun(t, pub, rel)
	s.run("TEAMFLOW_BIN_DIR=" + ro)
	s.wantRefused("cannot write to " + ro)
	if b, _ := os.ReadFile(filepath.Join(ro, "teamflow")); string(b) != oldScript {
		t.Fatal("the old binary changed")
	}
}

// The committed install.sh as it is (the placeholder key while there is none): with minisign present it still
// cannot check the signature, and says so plainly.
func TestInstallShPlaceholderKey(t *testing.T) {
	if PublicKey != "" {
		t.Skip("the release key is committed")
	}
	sk, _ := throwawayKey(t)
	s := newShRun(t, "", newShRelease(t, sk, runtime.GOOS, runtime.GOARCH))
	s.fake("minisign", "#!/bin/sh\nexit 0\n")
	s.run()
	s.wantOK()
	if !strings.Contains(s.out, "carries no release key yet; only the checksum was checked") {
		t.Fatalf("output:\n%s", s.out)
	}
}

// An https Releases page: every curl (the /latest lookup and each download) is held to https, redirects included
// (--proto '=https' --proto-redir '=https'). A local http test server gets neither (curl would refuse it).
func TestInstallShCurlStaysOnHTTPS(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	log := filepath.Join(t.TempDir(), "curl.log")
	real := filepath.Join(t.TempDir(), "curl")
	s := newShRun(t, pub, rel)
	if err := os.Rename(filepath.Join(s.tools, "curl"), real); err != nil {
		t.Fatal(err)
	}
	s.fake("curl", "#!/bin/sh\necho \"$*\" >> "+log+"\nexec "+real+" \"$@\"\n")

	// https: the lookup and the downloads (pinned, so all three files are fetched) all carry both flags. The
	// address refuses connections, so each curl fails fast after being called.
	s.run("TEAMFLOW_RELEASES_URL=https://127.0.0.1:1", "TEAMFLOW_VERSION=v0.2.0")
	s.wantRefused("download failed")
	s.run("TEAMFLOW_RELEASES_URL=https://127.0.0.1:1")
	s.wantRefused("cannot find the latest release")
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		t.Fatalf("curl calls: %q", lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, "--proto =https --proto-redir =https") {
			t.Fatalf("a curl without https-only: %s", l)
		}
	}

	// The local http server: no https-only flags, and it installs.
	_ = os.Remove(log)
	s.run()
	s.wantOK()
	b, _ = os.ReadFile(log)
	if strings.Contains(string(b), "--proto") || strings.Count(string(b), "\n") != 4 {
		t.Fatalf("local curl calls:\n%s", b)
	}
}

// The http exception is http://127.0.0.1:<port> or http://localhost:<port> only: a userinfo trick, a look-alike
// host, a port that is not digits, no port, another scheme, or http elsewhere are refused before any download.
func TestInstallShRefusesLookAlikeLocalURLs(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	for _, u := range []string{
		"http://127.0.0.1:80@evil.example", "http://localhost:80@evil.example/x", "http://localhost.evil.example:80",
		"http://127.0.0.1.evil.example:80", "http://127.0.0.1:abc", "http://localhost:", "http://localhost",
		"http://example.com", "ftp://127.0.0.1:21", "https://user@example.com", "https://",
	} {
		s := newShRun(t, pub, rel)
		s.run("TEAMFLOW_RELEASES_URL=" + u)
		s.wantRefused("TEAMFLOW_RELEASES_URL must be an https URL")
	}
	if rel.requests != 0 {
		t.Fatalf("%d requests", rel.requests)
	}
}

// Interrupted while copying into the bin folder (TERM after the temporary file is made): the script exits 130 and
// the temporary file is removed with the download folder; the old binary is untouched.
func TestInstallShInterruptLeavesNothing(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShRelease(t, sk, runtime.GOOS, runtime.GOARCH)
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "teamflow"), []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpRoot := t.TempDir()
	s := newShRun(t, pub, rel)
	realCp, _ := os.Readlink(filepath.Join(s.tools, "cp"))
	s.fake("cp", "#!/bin/sh\n"+realCp+" \"$@\"\nkill -TERM $PPID\nsleep 2\n")
	s.run("TEAMFLOW_BIN_DIR="+dir, "TMPDIR="+tmpRoot)
	if s.code != 130 {
		t.Fatalf("exit %d\n%s", s.code, s.out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left in the bin folder: %v", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "teamflow")); string(b) != oldScript {
		t.Fatal("the old binary changed")
	}
	if left, _ := os.ReadDir(tmpRoot); len(left) != 0 {
		t.Fatalf("left in TMPDIR: %v", left)
	}
}
