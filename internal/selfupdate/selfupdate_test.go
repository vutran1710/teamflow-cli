package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/minisign"
)

// ------------------------------------------------------------------ a fake release (throwaway keys, a TLS server)

const (
	oldScript = "#!/bin/sh\necho \"teamflow 0.1.0\"\n"
	newScript = "#!/bin/sh\necho \"teamflow 0.2.0\"\n"
)

// throwawayKey is a minisign keypair made for one test; never a release key.
func throwawayKey(t *testing.T) (minisign.SecretKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	b := []byte("Ed\x00\x00B2")
	b = append(b, make([]byte, 48)...)
	b = append(b, id[:]...)
	b = append(b, priv...)
	b = append(b, make([]byte, 32)...)
	sk, err := minisign.ParseSecretKey([]byte(base64.StdEncoding.EncodeToString(b)))
	if err != nil {
		t.Fatal(err)
	}
	return sk, sk.Public().String()
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type release struct {
	t        *testing.T
	srv      *httptest.Server
	tag      string
	assets   map[string][]byte // name -> served bytes
	sums     []byte            // checksums.txt as signed
	mu       sync.Mutex
	requests []string
	// knobs
	apiStatus  int               // non-zero: the API answers this
	apiHeaders map[string]string // with these headers
	apiBody    string            // non-empty: the API answers this body
	truncate   string            // asset served with half its bytes
	lieLength  string            // asset whose connection is cut short of its Content-Length
	plainURL   bool              // asset URLs given as http://
	redirectTo string            // asset whose https link redirects to plain http
	prerelease bool              // the latest release is marked a pre-release
}

// signed is the trusted comment the release workflow writes (tools/signsums).
func signed(tag string) string { return "timestamp:1\tfile:checksums.txt\ttag:" + tag }

func asset() string { return "teamflow_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz" }

// newRelease serves tag with an archive holding binary, a checksums.txt over every archive, and its signature by sk.
func newRelease(t *testing.T, sk minisign.SecretKey, tag, binary string) *release {
	t.Helper()
	r := &release{t: t, tag: tag, assets: map[string][]byte{}}
	arch := tarGz(t, map[string]string{"teamflow": binary, "LICENSE": "MIT\n"})
	r.assets[asset()] = arch
	other := "teamflow_linux_riscv64.tar.gz"
	r.assets[other] = tarGz(t, map[string]string{"teamflow": "x"})
	var sums strings.Builder
	for _, name := range []string{asset(), other} {
		h := sha256.Sum256(r.assets[name])
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	r.sums = []byte(sums.String())
	r.assets["checksums.txt"] = r.sums
	r.assets["checksums.txt.minisig"] = minisign.Sign(sk, r.sums, signed(tag))
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *release) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.URL.Path)
	r.mu.Unlock()
	if req.URL.Path == "/repos/vutran1710/teamflow-cli/releases/latest" {
		for k, v := range r.apiHeaders {
			w.Header().Set(k, v)
		}
		if r.apiStatus != 0 {
			w.WriteHeader(r.apiStatus)
			fmt.Fprint(w, `{"message": "API rate limit exceeded"}`)
			return
		}
		if r.apiBody != "" {
			fmt.Fprint(w, r.apiBody)
			return
		}
		type a struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int    `json:"size"`
		}
		var as []a
		base := r.srv.URL
		if r.plainURL {
			base = strings.Replace(base, "https://", "http://", 1)
		}
		for name, body := range r.assets {
			as = append(as, a{Name: name, URL: base + "/dl/" + name, Size: len(body)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": r.tag, "prerelease": r.prerelease, "assets": as})
		return
	}
	name := strings.TrimPrefix(req.URL.Path, "/dl/")
	body, ok := r.assets[name]
	if !ok || !strings.HasPrefix(req.URL.Path, "/dl/") {
		http.NotFound(w, req)
		return
	}
	if name == r.redirectTo && req.URL.Query().Get("hop") == "" {
		http.Redirect(w, req, strings.Replace(r.srv.URL, "https://", "http://", 1)+"/dl/"+name+"?hop=1", http.StatusFound)
		return
	}
	switch name {
	case r.truncate:
		_, _ = w.Write(body[:len(body)/2])
	case r.lieLength:
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body[:len(body)/2])
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	default:
		_, _ = w.Write(body)
	}
}

func (r *release) fetched(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.requests {
		if p == "/dl/"+name {
			return true
		}
	}
	return false
}

func (r *release) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// install puts the old binary in a fresh folder and returns an Updater aimed at the fake release.
func (r *release) updater(t *testing.T, pub string) (*Updater, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "teamflow")
	if err := os.WriteFile(exe, []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return &Updater{API: r.srv.URL, Client: r.srv.Client(), PublicKey: pub, Exe: exe, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Out: out}, exe, out
}

// untouched checks the old binary is still there, as it was, and nothing else is left in its folder.
func untouched(t *testing.T, exe string) {
	t.Helper()
	b, err := os.ReadFile(exe)
	if err != nil || string(b) != oldScript {
		t.Fatalf("the old binary changed: %q %v", b, err)
	}
	leftovers(t, filepath.Dir(exe), "teamflow")
}

func leftovers(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("folder holds %v, want %v", names, want)
	}
}

func wantRefusal(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil {
		t.Fatalf("not refused (want %q)", text)
	}
	if !strings.Contains(err.Error(), text) {
		t.Fatalf("refusal %q does not say %q", err, text)
	}
}

// ------------------------------------------------------------------ tests

// Happy path: a newer signed release replaces the binary in place (same folder, same mode), nothing left behind.
func TestSelfUpdateReplacesBinary(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, out := rel.updater(t, pub)
	replaced := 0
	u.Replaced = func() {
		replaced++
		if b, _ := os.ReadFile(exe); string(b) != newScript { // told only once the new binary is in place
			t.Error("Replaced before the binary was replaced")
		}
	}
	if err := u.Update(context.Background(), "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if replaced != 1 {
		t.Fatalf("Replaced called %d times", replaced)
	}
	b, _ := os.ReadFile(exe)
	if string(b) != newScript {
		t.Fatalf("binary is %q", b)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	leftovers(t, filepath.Dir(exe), "teamflow")
	if !strings.Contains(out.String(), "0.1.0 -> 0.2.0") {
		t.Fatalf("out: %s", out)
	}
	if u.Client.CheckRedirect != nil {
		t.Fatal("Update changed the caller's client (it works on a copy)")
	}
}

// Up to date (or a newer build than the latest release): nothing is downloaded or changed. A dev build updates.
func TestSelfUpdateUpToDate(t *testing.T) {
	sk, pub := throwawayKey(t)
	for _, current := range []string{"0.2.0", "v0.2.0", "0.3.0"} {
		rel := newRelease(t, sk, "v0.2.0", newScript)
		u, exe, out := rel.updater(t, pub)
		u.Replaced = func() { t.Errorf("%s: Replaced with nothing to update", current) }
		if err := u.Update(context.Background(), current); err != nil {
			t.Fatalf("%s: %v", current, err)
		}
		untouched(t, exe)
		if rel.fetched(asset()) || !strings.Contains(out.String(), "nothing to update") {
			t.Fatalf("%s: fetched=%v out=%s", current, rel.fetched(asset()), out)
		}
	}
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	if err := u.Update(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != newScript {
		t.Fatal("a dev build was not updated")
	}
}

// Sad paths: signed by another key; signed by another key under the same id; checksums.txt changed after signing;
// a damaged signature; no signature in the release. Each leaves the old binary and downloads no archive.
func TestSelfUpdateRefusesBadSignature(t *testing.T) {
	sk, pub := throwawayKey(t)
	other, _ := throwawayKey(t)
	cases := map[string]struct {
		edit func(r *release)
		want string
	}{
		"another key": {func(r *release) {
			r.assets["checksums.txt.minisig"] = minisign.Sign(other, r.sums, "timestamp:1\tfile:checksums.txt")
		}, "signed by another key"},
		"same id, another key": {func(r *release) {
			b := []byte("Ed\x00\x00B2")
			b = append(b, make([]byte, 48)...)
			b = append(b, sk.ID[:]...)
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			b = append(b, priv...)
			b = append(b, make([]byte, 32)...)
			imp, _ := minisign.ParseSecretKey([]byte(base64.StdEncoding.EncodeToString(b)))
			r.assets["checksums.txt.minisig"] = minisign.Sign(imp, r.sums, "timestamp:1\tfile:checksums.txt")
		}, "signature does not match"},
		"checksums changed": {func(r *release) {
			r.assets["checksums.txt"] = append(bytes.Clone(r.sums), "0000  extra\n"...)
		}, "signature does not match"},
		"damaged signature": {func(r *release) {
			r.assets["checksums.txt.minisig"] = []byte("untrusted comment: x\nnot base64\n")
		}, "signature"},
		"no signature": {func(r *release) { delete(r.assets, "checksums.txt.minisig") }, "checksums.txt.minisig"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, sk, "v0.2.0", newScript)
			c.edit(rel)
			u, exe, _ := rel.updater(t, pub)
			u.Replaced = func() { t.Error("Replaced on a refused update") }
			wantRefusal(t, u.Update(context.Background(), "0.1.0"), c.want)
			untouched(t, exe)
			if rel.fetched(asset()) {
				t.Fatal("downloaded the archive before the signature was checked")
			}
		})
	}
}

// Sad paths: the archive does not match its signed checksum; the signed checksums have no line for this build.
func TestSelfUpdateRefusesBadChecksum(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	rel.assets[asset()] = tarGz(t, map[string]string{"teamflow": "#!/bin/sh\necho evil\n"})
	u, exe, _ := rel.updater(t, pub)
	wantRefusal(t, u.Update(context.Background(), "0.1.0"), "checksum")
	untouched(t, exe)

	rel = newRelease(t, sk, "v0.2.0", newScript)
	var sums strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(rel.sums)), "\n") {
		if !strings.HasSuffix(line, asset()) {
			sums.WriteString(line + "\n")
		}
	}
	rel.sums = []byte(sums.String())
	rel.assets["checksums.txt"] = rel.sums
	rel.assets["checksums.txt.minisig"] = minisign.Sign(sk, rel.sums, signed("v0.2.0"))
	u, exe, _ = rel.updater(t, pub)
	wantRefusal(t, u.Update(context.Background(), "0.1.0"), "no checksum for "+asset())
	untouched(t, exe)
}

// Sad paths: a truncated download (short body; a connection cut before its Content-Length), no build for this
// OS/arch, GitHub's rate limit, GitHub unreachable, no release yet, a malformed API answer, an archive without the
// binary, a binary that does not run here (or names another version), and a plain-http download link. Every one
// leaves the old binary as it was and nothing else in its folder.
func TestSelfUpdateKeepsOldBinaryOnFailure(t *testing.T) {
	sk, pub := throwawayKey(t)
	cases := map[string]struct {
		edit func(r *release, u *Updater)
		want string
	}{
		"truncated":                  {func(r *release, _ *Updater) { r.truncate = asset() }, "checksum"},
		"connection cut":             {func(r *release, _ *Updater) { r.lieLength = asset() }, "download"},
		"no build for this platform": {func(_ *release, u *Updater) { u.GOARCH = "mips" }, "no build for " + runtime.GOOS + "/mips"},
		"rate limited": {func(r *release, _ *Updater) {
			r.apiStatus = http.StatusForbidden
			r.apiHeaders = map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1900000000"}
		}, "rate limit"},
		"too many requests": {func(r *release, _ *Updater) { r.apiStatus = http.StatusTooManyRequests }, "rate limit"},
		"no release":        {func(r *release, _ *Updater) { r.apiStatus = http.StatusNotFound }, "no release"},
		"server error":      {func(r *release, _ *Updater) { r.apiStatus = http.StatusBadGateway }, "502"},
		"malformed answer":  {func(r *release, _ *Updater) { r.apiBody = `{"tag_name": ` }, "GitHub's answer"},
		"no tag":            {func(r *release, _ *Updater) { r.apiBody = `{"assets": []}` }, "GitHub's answer"},
		"offline": {func(_ *release, u *Updater) {
			l, _ := net.Listen("tcp", "127.0.0.1:0")
			u.API = "https://" + l.Addr().String()
			_ = l.Close()
		}, "cannot reach GitHub"},
		"archive without binary": {func(r *release, _ *Updater) {
			r.assets[asset()] = tarGz(t, map[string]string{"README": "x"})
			r.resign(sk)
		}, "holds no teamflow binary"},
		"binary does not run": {func(r *release, _ *Updater) {
			r.assets[asset()] = tarGz(t, map[string]string{"teamflow": "\x7fELF garbage"})
			r.resign(sk)
		}, "does not run"},
		"binary names another version": {func(r *release, _ *Updater) {
			r.assets[asset()] = tarGz(t, map[string]string{"teamflow": "#!/bin/sh\necho \"teamflow 0.0.9\"\n"})
			r.resign(sk)
		}, "not v0.2.0"},
		"plain http link":  {func(r *release, _ *Updater) { r.plainURL = true }, "not https"},
		"redirect to http": {func(r *release, _ *Updater) { r.redirectTo = asset() }, "not https"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, sk, "v0.2.0", newScript)
			u, exe, _ := rel.updater(t, pub)
			c.edit(rel, u)
			wantRefusal(t, u.Update(context.Background(), "0.1.0"), c.want)
			untouched(t, exe)
		})
	}
}

// resign rewrites checksums.txt over the served archives and signs it (a release whose archive is bad but honest).
func (r *release) resign(sk minisign.SecretKey) {
	h := sha256.Sum256(r.assets[asset()])
	r.sums = []byte(hex.EncodeToString(h[:]) + "  " + asset() + "\n")
	r.assets["checksums.txt"] = r.sums
	r.assets["checksums.txt.minisig"] = minisign.Sign(sk, r.sums, signed(r.tag))
}

// The placeholder key (this build has no release key): refused before any network call, nothing changed.
func TestSelfUpdateRefusesWithoutReleaseKey(t *testing.T) {
	sk, _ := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	for _, key := range []string{"", "  ", "not-a-key"} {
		u, exe, _ := rel.updater(t, key)
		wantRefusal(t, u.Update(context.Background(), "0.1.0"), "this build has no release key")
		untouched(t, exe)
	}
	if rel.count() != 0 {
		t.Fatalf("sent %d requests", rel.count())
	}
}

// Sad paths: the binary's folder is not writable (a read-only install location): refused before any download.
func TestSelfUpdateRefusesReadOnlyFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	dir, _ := filepath.EvalSymlinks(filepath.Dir(exe))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	wantRefusal(t, u.Update(context.Background(), "0.1.0"), "cannot write to "+dir)
	untouched(t, exe)
	if rel.count() != 0 {
		t.Fatalf("sent %d requests", rel.count())
	}
}

// A binary reached through a symlink: the file the link points to is replaced; the link stays a link.
// A dangling link is refused.
func TestSelfUpdateFollowsSymlink(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, target, _ := rel.updater(t, pub)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "teamflow")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	u.Exe = link
	if err := u.Update(context.Background(), "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != newScript {
		t.Fatalf("target is %q", b)
	}
	leftovers(t, filepath.Dir(target), "teamflow")

	u.Exe = filepath.Join(linkDir, "dangling")
	if err := os.Symlink(filepath.Join(linkDir, "gone"), u.Exe); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, u.Update(context.Background(), "0.1.0"), "cannot find the running binary")
}

// The committed key is the placeholder or a valid minisign public key, and install.sh carries the same one.
func TestPublicKeyPlaceholderOrValid(t *testing.T) {
	if PublicKey != "" {
		if _, err := minisign.ParsePublicKey(PublicKey); err != nil {
			t.Fatalf("pubkey.go: %v", err)
		}
	}
	sh, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sh), "\n\tPUBKEY=\""+PublicKey+"\"\n") {
		t.Fatalf("install.sh's PUBKEY is not pubkey.go's %q", PublicKey)
	}
}

// The signed trusted comment names the release's tag; a signature made for another release (an older release's
// checksums.txt and signature served as the latest: a rollback), one naming no tag, or one naming a longer tag is
// refused before the archive is downloaded.
func TestSelfUpdateRefusesSignatureForAnotherTag(t *testing.T) {
	sk, pub := throwawayKey(t)
	for name, c := range map[string]struct{ comment, want string }{
		"older release": {signed("v0.1.0"), "signed for v0.1.0, not v0.2.0"},
		"no tag":        {"timestamp:1\tfile:checksums.txt", "names no release tag"},
		"longer tag":    {signed("v0.2.0-evil"), "signed for v0.2.0-evil, not v0.2.0"},
		"two tags":      {signed("v0.2.0") + "\ttag:v0.1.0", "names more than one release tag"},
	} {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, sk, "v0.2.0", newScript)
			rel.assets["checksums.txt.minisig"] = minisign.Sign(sk, rel.sums, c.comment)
			u, exe, _ := rel.updater(t, pub)
			wantRefusal(t, u.Update(context.Background(), "0.1.0"), c.want)
			untouched(t, exe)
			if rel.fetched(asset()) {
				t.Fatal("downloaded the archive")
			}
		})
	}
}

// Install says what it did: the versions, whether the binary was replaced, and which file.
func TestInstallResult(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	res, err := u.Install(context.Background(), "0.1.0")
	real, _ := filepath.EvalSymlinks(exe)
	if err != nil || res != (Result{Current: "0.1.0", Latest: "v0.2.0", Updated: true, Exe: real}) {
		t.Fatalf("%+v %v", res, err)
	}
	res, err = u.Install(context.Background(), "0.2.0")
	if err != nil || res.Updated || res.Latest != "v0.2.0" {
		t.Fatalf("up to date: %+v %v", res, err)
	}
	if tag, err := u.Latest(context.Background()); tag != "v0.2.0" || err != nil {
		t.Fatalf("Latest %q %v", tag, err)
	}
}

// Never a downgrade, never a pre-release, never a tag that is not a release version: nothing downloaded or changed.
func TestInstallSkipsPrereleasesAndDowngrades(t *testing.T) {
	sk, pub := throwawayKey(t)
	for name, c := range map[string]struct {
		tag, current string
		pre          bool
	}{
		"older release":       {"v0.1.0", "0.2.0", false},
		"same release":        {"v0.2.0", "v0.2.0", false},
		"marked pre-release":  {"v0.3.0", "0.2.0", true},
		"a pre-release tag":   {"v0.3.0-rc1", "0.2.0", false},
		"not a version":       {"nightly", "0.2.0", false},
		"pre-release for dev": {"v0.3.0-rc1", "dev", false},
	} {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, sk, c.tag, newScript)
			rel.prerelease = c.pre
			u, exe, _ := rel.updater(t, pub)
			res, err := u.Install(context.Background(), c.current)
			if err != nil || res.Updated {
				t.Fatalf("%+v %v", res, err)
			}
			untouched(t, exe)
			if rel.fetched(asset()) || rel.fetched("checksums.txt") {
				t.Fatal("downloaded a release it does not install")
			}
		})
	}
}

// Each refusal says why, as a Kind: offline, rate limited, not writable, and every check of the release.
func TestInstallRefusalKinds(t *testing.T) {
	sk, pub := throwawayKey(t)
	other, _ := throwawayKey(t)
	cases := map[string]struct {
		edit func(r *release, u *Updater)
		kind Kind
	}{
		"offline": {func(_ *release, u *Updater) {
			l, _ := net.Listen("tcp", "127.0.0.1:0")
			u.API = "https://" + l.Addr().String()
			_ = l.Close()
		}, Offline},
		"rate limited": {func(r *release, _ *Updater) { r.apiStatus = http.StatusTooManyRequests }, RateLimited},
		"bad signature": {func(r *release, _ *Updater) {
			r.assets["checksums.txt.minisig"] = minisign.Sign(other, r.sums, signed(r.tag))
		}, Unverified},
		"wrong tag": {func(r *release, _ *Updater) {
			r.assets["checksums.txt.minisig"] = minisign.Sign(sk, r.sums, signed("v0.1.9"))
		}, Unverified},
		"bad checksum":   {func(r *release, _ *Updater) { r.assets[asset()] = tarGz(t, map[string]string{"teamflow": "evil"}) }, Unverified},
		"unsigned":       {func(r *release, _ *Updater) { delete(r.assets, "checksums.txt.minisig") }, Unverified},
		"no release key": {func(_ *release, u *Updater) { u.PublicKey = "" }, Unverified},
		"no release yet": {func(r *release, _ *Updater) { r.apiStatus = http.StatusNotFound }, Failed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, sk, "v0.2.0", newScript)
			u, exe, _ := rel.updater(t, pub)
			c.edit(rel, u)
			res, err := u.Install(context.Background(), "0.1.0")
			if err == nil || KindOf(err) != c.kind || res.Updated {
				t.Fatalf("kind %v (want %v): %+v %v", KindOf(err), c.kind, res, err)
			}
			untouched(t, exe)
		})
	}
	if os.Geteuid() != 0 {
		rel := newRelease(t, sk, "v0.2.0", newScript)
		u, exe, _ := rel.updater(t, pub)
		dir := filepath.Dir(exe)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if _, err := u.Install(context.Background(), "0.1.0"); KindOf(err) != NotWritable {
			t.Fatalf("read-only folder: %v", err)
		}
		if tag, err := u.Latest(context.Background()); tag != "v0.2.0" || err != nil { // the version can still be named
			t.Fatalf("Latest %q %v", tag, err)
		}
	}
	if KindOf(errors.New("plain")) != Failed {
		t.Fatal("a plain error is not Failed")
	}
}

// Hold is held while the new binary is renamed into place, after every check, and released after; a Hold that
// can't be taken (a send in flight past its wait) refuses: the old binary stays, nothing left beside it.
func TestInstallHoldsAroundTheSwap(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	var order []string
	u.Hold = func(context.Context) (func(), error) {
		b, _ := os.ReadFile(exe)
		order = append(order, "hold:"+strings.TrimSpace(string(b)))
		return func() {
			b, _ := os.ReadFile(exe)
			order = append(order, "release:"+strings.TrimSpace(string(b)))
		}, nil
	}
	if _, err := u.Install(context.Background(), "0.1.0"); err != nil {
		t.Fatal(err)
	}
	want := []string{"hold:" + strings.TrimSpace(oldScript), "release:" + strings.TrimSpace(newScript)}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("order %q", order)
	}

	rel = newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ = rel.updater(t, pub)
	u.Hold = func(context.Context) (func(), error) { return nil, errors.New("another teamflow is sending") }
	u.Replaced = func() { t.Error("Replaced without the hold") }
	_, err := u.Install(context.Background(), "0.1.0")
	if KindOf(err) != Failed || !strings.Contains(err.Error(), "another teamflow is sending") {
		t.Fatalf("%v", err)
	}
	untouched(t, exe)
}

func TestNewerAndIsRelease(t *testing.T) {
	for _, c := range []struct {
		tag, current string
		want         bool
	}{{"v0.1.1", "0.1.0", true}, {"v0.1.1", "v0.1.1", false}, {"v0.1.0", "0.1.1", false}, {"v0.10.0", "0.9.9", true},
		{"v1.0.0-rc1", "0.9.0", false}, {"v0.1.1", "dev", true}, {"nightly", "dev", false}} {
		if got := Newer(c.tag, c.current); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.tag, c.current, got)
		}
	}
	for v, want := range map[string]bool{"0.1.0": true, "v0.1.0": true, "dev": false, "ci-1a2b3c4": false, "0.1": false, "v1.0.0-rc1": false} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%s) != %v", v, want)
		}
	}
}

// Updates of one binary take turns (a lock file): a second waits for the first, and refuses past its wait, nothing
// changed.
func TestInstallTakesTurns(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	u.LockFile = filepath.Join(t.TempDir(), "update.lock")
	held := make(chan struct{})
	release := make(chan struct{})
	u.Hold = func(context.Context) (func(), error) { close(held); <-release; return func() {}, nil }
	done := make(chan error, 1)
	go func() { _, err := u.Install(context.Background(), "0.1.0"); done <- err }()
	<-held
	second, exe2, _ := rel.updater(t, pub)
	second.LockFile, second.LockWait = u.LockFile, 100*time.Millisecond
	_, err := second.Install(context.Background(), "0.1.0")
	if err == nil || !strings.Contains(err.Error(), "another teamflow update is running") {
		t.Fatalf("the second update did not wait its turn: %v", err)
	}
	untouched(t, exe2)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != newScript {
		t.Fatal("the first update did not finish")
	}
	second.LockWait = 5 * time.Second // its turn now
	if _, err := second.Install(context.Background(), "0.1.0"); err != nil {
		t.Fatal(err)
	}
}

// The binary is put in place only if the file on disk is still the one the update started from (and what Expect
// says): one replaced meanwhile (another install) stays, nothing changed.
func TestInstallNeverReplacesABinaryChangedMeanwhile(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	other := "#!/bin/sh\necho \"teamflow dev\"\n"
	u.Hold = func(context.Context) (func(), error) {
		tmp := exe + ".other"
		if err := os.WriteFile(tmp, []byte(other), 0o755); err != nil {
			return nil, err
		}
		return func() {}, os.Rename(tmp, exe) // another install renamed a binary into place meanwhile
	}
	_, err := u.Install(context.Background(), "0.1.0")
	if KindOf(err) != Changed {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != other {
		t.Fatalf("the binary is %q", b)
	}
	leftovers(t, filepath.Dir(exe), "teamflow")

	// Expect refuses: before anything is downloaded, and again just before the rename.
	rel = newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ = rel.updater(t, pub)
	u.Expect = func(string) error { return errors.New("not the binary this sync started from") }
	if _, err := u.Install(context.Background(), "0.1.0"); KindOf(err) != Changed || rel.count() != 0 {
		t.Fatalf("%v; %d requests", err, rel.count())
	}
	untouched(t, exe)
	calls := 0
	u.Expect = func(string) error {
		calls++
		if calls > 1 {
			return errors.New("changed while downloading")
		}
		return nil
	}
	if _, err := u.Install(context.Background(), "0.1.0"); KindOf(err) != Changed || calls != 2 {
		t.Fatalf("%v; Expect called %d times", err, calls)
	}
	untouched(t, exe)
}

// ExpectVersion: the binary on disk must still say it is this version.
func TestExpectVersion(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "teamflow")
	if err := os.WriteFile(exe, []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ExpectVersion("0.1.0")(exe); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0.2.0", "dev"} {
		if err := ExpectVersion(v)(exe); err == nil || !strings.Contains(err.Error(), "teamflow 0.1.0") {
			t.Fatalf("%s: %v", v, err)
		}
	}
	if err := ExpectVersion("0.1.0")(filepath.Join(dir, "gone")); err == nil {
		t.Fatal("a missing binary passed")
	}
}

// Temporary files an interrupted update left beside the binary are swept when older than an hour.
func TestInstallSweepsOldTempFiles(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newRelease(t, sk, "v0.2.0", newScript)
	u, exe, _ := rel.updater(t, pub)
	dir := filepath.Dir(exe)
	old, fresh := filepath.Join(dir, ".teamflow-update-111"), filepath.Join(dir, ".teamflow-update-222")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ago := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, ago, ago); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Install(context.Background(), "0.2.0"); err != nil { // nothing to update: swept all the same
		t.Fatal(err)
	}
	leftovers(t, dir, ".teamflow-update-222", "teamflow")
}
