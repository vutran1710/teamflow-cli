// Package selfupdatetest serves a fake teamflow release for tests of what runs package selfupdate (/teamf upgrade,
// the background sync's scheduled update): a throwaway minisign key (never a release key), and a TLS test server
// answering GitHub's /repos/<repo>/releases/latest and the release's files. Nothing reaches GitHub.
package selfupdatetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/minisign"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
)

// Script is a stand-in teamflow binary: a shell script answering --version as a build of version would.
func Script(version string) string {
	return "#!/bin/sh\necho \"teamflow " + strings.TrimPrefix(version, "v") + "\"\n"
}

// Installed writes a stand-in binary of version as dir/teamflow (0755) and returns its path.
func Installed(t *testing.T, dir, version string) string {
	t.Helper()
	exe := filepath.Join(dir, "teamflow")
	if err := os.WriteFile(exe, []byte(Script(version)), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

// Release is one fake release.
type Release struct {
	t         *testing.T
	srv       *httptest.Server
	Tag       string
	PublicKey string // the throwaway key's public half, for Updater.PublicKey

	mu         sync.Mutex
	sk         minisign.SecretKey
	assets     map[string][]byte
	sums       []byte
	apiStatus  int
	prerelease bool
	requests   []string
}

// New serves tag with an archive for this platform holding binary (Script(tag) when ""), checksums.txt over it and
// its signature, signed for tag.
func New(t *testing.T, tag, binary string) *Release {
	t.Helper()
	if binary == "" {
		binary = Script(tag)
	}
	sk, pub := throwawayKey(t)
	r := &Release{t: t, Tag: tag, PublicKey: pub, sk: sk, assets: map[string][]byte{}}
	r.assets[Asset()] = tarGz(t, map[string]string{"teamflow": binary, "LICENSE": "MIT\n"})
	r.resign(tag)
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// Asset is this platform's archive name.
func Asset() string { return selfupdate.AssetName(runtime.GOOS, runtime.GOARCH) }

// Updater is an Updater of exe aimed at this release (its key, its server), saying what it does to out.
func (r *Release) Updater(exe string, out io.Writer) *selfupdate.Updater {
	if out == nil {
		out = io.Discard
	}
	return &selfupdate.Updater{API: r.srv.URL, Client: r.srv.Client(), PublicKey: r.PublicKey, Exe: exe,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Out: out}
}

// SignedByAnotherKey re-signs checksums.txt with another throwaway key: the signature check must refuse it.
func (r *Release) SignedByAnotherKey() {
	other, _ := throwawayKey(r.t)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assets["checksums.txt.minisig"] = minisign.Sign(other, r.sums, signed(r.Tag))
}

// Answer makes GitHub's API answer status (429: rate limited; 0: the release again).
func (r *Release) Answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apiStatus = status
}

// Prerelease marks the release a pre-release.
func (r *Release) Prerelease() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prerelease = true
}

// Asked is how many times GitHub's API was asked for the latest release.
func (r *Release) Asked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.requests {
		if strings.HasPrefix(p, "/repos/") {
			n++
		}
	}
	return n
}

// Downloaded says whether the archive was downloaded.
func (r *Release) Downloaded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.requests {
		if p == "/dl/"+Asset() {
			return true
		}
	}
	return false
}

// OfflineUpdater is an Updater of exe whose GitHub is a closed port: it can't be reached.
func OfflineUpdater(t *testing.T, exe string) *selfupdate.Updater {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, pub := throwawayKey(t)
	return &selfupdate.Updater{API: "https://" + addr, Client: &http.Client{}, PublicKey: pub, Exe: exe,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Out: io.Discard}
}

func (r *Release) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.URL.Path)
	status, pre := r.apiStatus, r.prerelease
	assets := map[string][]byte{}
	for k, v := range r.assets {
		assets[k] = v
	}
	r.mu.Unlock()
	if req.URL.Path == "/repos/"+selfupdate.Repo+"/releases/latest" {
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message": "API rate limit exceeded"}`)
			return
		}
		type a struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		var as []a
		for name := range assets {
			as = append(as, a{Name: name, URL: r.srv.URL + "/dl/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": r.Tag, "prerelease": pre, "assets": as})
		return
	}
	body, ok := assets[strings.TrimPrefix(req.URL.Path, "/dl/")]
	if !ok || !strings.HasPrefix(req.URL.Path, "/dl/") {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

// resign writes checksums.txt over the archive and signs it for tag.
func (r *Release) resign(tag string) {
	h := sha256.Sum256(r.assets[Asset()])
	r.sums = []byte(hex.EncodeToString(h[:]) + "  " + Asset() + "\n")
	r.assets["checksums.txt"] = r.sums
	r.assets["checksums.txt.minisig"] = minisign.Sign(r.sk, r.sums, signed(tag))
}

// signed is the trusted comment the release workflow writes.
func signed(tag string) string { return "timestamp:1\tfile:checksums.txt\ttag:" + tag }

// throwawayKey is a minisign keypair made for one test.
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
