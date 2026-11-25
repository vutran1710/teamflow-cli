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

type shRelease struct {
	srv      *httptest.Server
	files    map[string][]byte
	mu       sync.Mutex
	requests int
}

func newShRelease(t *testing.T, sk minisign.SecretKey, osName, arch string) *shRelease {
	t.Helper()
	r := &shRelease{files: map[string][]byte{}}
	name := AssetName(osName, arch)
	r.files[name] = tarGz(t, map[string]string{"teamflow": markScript, "LICENSE": "MIT\n"})
	h := sha256.Sum256(r.files[name])
	sums := []byte(hex.EncodeToString(h[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  teamflow_other.tar.gz\n")
	r.files["checksums.txt"] = sums
	r.files["checksums.txt.minisig"] = minisign.Sign(sk, sums, "timestamp:1\tfile:checksums.txt")
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests++
		b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/download/")]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
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
	text := strings.Replace(string(src), "\nPUBKEY=\""+PublicKey+"\"\n", "\nPUBKEY=\""+pub+"\"\n", 1)
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
	if err := os.WriteFile(filepath.Join(s.tools, name), []byte(script), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *shRun) run(extra ...string) {
	s.t.Helper()
	cmd := exec.Command(filepath.Join(s.tools, "sh"), s.script)
	cmd.Env = append([]string{"HOME=" + s.home, "PATH=" + s.tools, "MARK=" + s.mark, "TEAMFLOW_DOWNLOAD_URL=" + s.release.srv.URL + "/download"}, extra...)
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
	s.fake("minisign", "#!/bin/sh\necho \"$@\" > "+args+"\nexit ${FAKE_MINISIGN_EXIT:-0}\n")
	s.run()
	s.wantOK()
	if b, _ := os.ReadFile(args); strings.TrimSpace(string(b)) != "-Vm checksums.txt -x checksums.txt.minisig -P "+pub {
		t.Fatalf("minisign args %q", b)
	}
	if strings.Contains(s.out, "only the checksum") || !strings.Contains(s.out, "signature verified") {
		t.Fatalf("output:\n%s", s.out)
	}

	s = newShRun(t, pub, rel)
	s.fake("minisign", "#!/bin/sh\nexit 1\n")
	s.run()
	s.wantRefused("signature check failed")

	delete(rel.files, "checksums.txt.minisig")
	s = newShRun(t, pub, rel)
	s.run()
	s.wantRefused("checksums.txt.minisig")
	s = newShRun(t, pub, rel)
	s.fake("minisign", "#!/bin/sh\nexit 0\n")
	s.run()
	s.wantRefused("checksums.txt.minisig")
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
	rel.files["checksums.txt.minisig"] = minisign.Sign(other, rel.files["checksums.txt"], "timestamp:1\tfile:checksums.txt")
	s = newShRun(t, pub, rel)
	if err := os.Symlink(real, filepath.Join(s.tools, "minisign")); err != nil {
		t.Fatal(err)
	}
	s.run()
	s.wantRefused("signature check failed")
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
