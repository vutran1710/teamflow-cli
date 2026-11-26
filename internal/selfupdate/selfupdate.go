// Package selfupdate replaces the running teamflow binary with the newest signed release from GitHub.
//
// Order: find the binary and check its folder is writable; ask GitHub for the latest release; download
// checksums.txt and its minisign signature and check the signature with the compiled-in key; download this
// platform's archive and check its SHA-256 against the signed list; take the binary out of the archive into a
// temporary file beside the old one; check it runs and names the release's version; rename it over the old one
// (atomic in one folder). Any failure refuses and leaves the old binary as it was.
//
// A binary reached through a symlink: the file the link resolves to is replaced, and the link is left alone.
//
// Only a newer release is installed: never the same or an older one (no downgrade), never a pre-release or a draft,
// never a tag that is not a release version (vX.Y.Z). A build that is not a release version itself (dev) takes the
// latest release. `teamflow self-update`, /teamf upgrade and the background sync's scheduled update all run this.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/minisign"
)

const (
	// Repo is where releases are published.
	Repo = "vutran1710/teamflow-cli"
	// ReleasesPage is where a person downloads a release by hand.
	ReleasesPage = "https://github.com/" + Repo + "/releases"

	maxAPI       = 1 << 20
	maxSums      = 64 << 10
	maxSig       = 4 << 10
	maxArchive   = 256 << 20
	maxBinary    = 256 << 20
	checkTimeout = 10 * time.Second
)

// Updater is one self-update; the zero values of its fields are filled by Default.
type Updater struct {
	API       string       // GitHub's API root
	Client    *http.Client // for the API and the downloads
	PublicKey string       // the release key (minisign, one line)
	Exe       string       // the running binary
	GOOS      string
	GOARCH    string
	Out       io.Writer // what is said on success
	Replaced  func()    // when set, called once the new binary is in place (never when nothing changed)
	// Hold, when set, is held while the new binary is renamed into place (the background sync takes the outbox's
	// sender locks: never a swap in the middle of a send). An error refuses the update, nothing changed.
	Hold func(ctx context.Context) (release func(), err error)
	// Expect, when set, says what the binary on disk must still be: checked before anything is downloaded and
	// again, under Hold, just before the new binary is renamed into place. An error refuses the update (Changed).
	Expect func(exe string) error
	// LockFile, when set, is held (flock) for the whole update, so two updates of the binary (self-update, /teamf
	// upgrade, the background sync's) take turns; LockWait is how long one waits for another (2 min).
	LockFile string
	LockWait time.Duration
}

// lockWait is how long an update waits for another one by default.
const lockWait = 2 * time.Minute

// staleTemp: a temporary file of an interrupted update older than this is swept.
const staleTemp = time.Hour

// Result is what an update did.
type Result struct {
	Current string // the running build's version, as given
	Latest  string // the newest release's tag ("v0.2.0"; "" when GitHub was not reached)
	Updated bool   // the binary was replaced by Latest's
	Exe     string // the file replaced (a link resolved)
}

// Kind is why an update was refused.
type Kind int

const (
	Failed      Kind = iota // any other reason
	Offline                 // GitHub could not be reached
	RateLimited             // GitHub's API rate limit for this network
	NotWritable             // the binary's folder can't be written
	Unverified              // the signature, the tag it was signed for or the checksum did not check out
	Changed                 // the binary on disk is not the one the update started from (another install ran)
)

// Error is a refused update; nothing was changed.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// KindOf is why err refused an update (Failed for an error that is not an *Error).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Failed
}

func refuse(k Kind, err error) error { return &Error{Kind: k, Err: err} }

// Default is the updater for the running binary; its updates take turns with the Teamflow home's other ones.
func Default(out io.Writer) *Updater {
	exe, _ := os.Executable()
	return &Updater{
		LockFile:  filepath.Join(home.Dir(), "update.lock"),
		API:       "https://api.github.com",
		Client:    &http.Client{Timeout: 5 * time.Minute},
		PublicKey: PublicKey,
		Exe:       exe,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		Out:       out,
	}
}

// Run updates the running binary (built as version current) to the latest release.
func Run(ctx context.Context, current string) error {
	return Default(os.Stdout).Update(ctx, current)
}

// AssetName is a platform's archive in a release.
func AssetName(goos, goarch string) string { return "teamflow_" + goos + "_" + goarch + ".tar.gz" }

type ghRelease struct {
	Tag        string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Update replaces u.Exe with the latest release, or refuses and leaves it as it was.
func (u *Updater) Update(ctx context.Context, current string) error {
	_, err := u.Install(ctx, current)
	return err
}

// Install replaces u.Exe with the latest release when it is newer than current, or refuses (an *Error) and leaves
// it as it was.
func (u *Updater) Install(ctx context.Context, current string) (Result, error) {
	res := Result{Current: current}
	key, err := minisign.ParsePublicKey(u.PublicKey)
	if strings.TrimSpace(u.PublicKey) == "" || err != nil {
		return res, refuse(Unverified, fmt.Errorf("this build has no release key, so it cannot check a release's signature; download the new release from %s", ReleasesPage))
	}
	exe, err := filepath.EvalSymlinks(u.Exe)
	if err != nil || u.Exe == "" {
		return res, refuse(Failed, errors.New("cannot find the running binary on disk; download the new release from "+ReleasesPage))
	}
	res.Exe = exe
	dir := filepath.Dir(exe)
	if err := writable(dir); err != nil {
		return res, refuse(NotWritable, fmt.Errorf("cannot write to %s (where teamflow is installed): %v; reinstall with install.sh, or run self-update as a user who can write there", dir, err))
	}
	sweep(dir)
	unlock, err := u.takeTurn(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	start, err := os.Stat(exe) // the binary this update replaces: still it at the rename, or nothing changes
	if err != nil {
		return res, refuse(Failed, fmt.Errorf("cannot read %s: %v", exe, err))
	}
	if u.Expect != nil {
		if err := u.Expect(exe); err != nil {
			return res, refuse(Changed, err)
		}
	}
	u = u.withClient()
	rel, err := u.latest(ctx)
	if err != nil {
		return res, err
	}
	res.Latest = rel.Tag
	if rel.Prerelease || rel.Draft {
		fmt.Fprintf(u.Out, "the latest release (%s) is a pre-release; teamflow %s stays\n", rel.Tag, current)
		return res, nil
	}
	if !newer(rel.Tag, current) {
		fmt.Fprintf(u.Out, "teamflow %s is the latest release (%s); nothing to update\n", current, rel.Tag)
		return res, nil
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	name := AssetName(u.GOOS, u.GOARCH)
	if urls[name] == "" {
		return res, refuse(Failed, fmt.Errorf("release %s has no build for %s/%s", rel.Tag, u.GOOS, u.GOARCH))
	}
	for _, need := range []string{"checksums.txt", "checksums.txt.minisig"} {
		if urls[need] == "" {
			return res, refuse(Unverified, fmt.Errorf("release %s has no %s; refusing an unsigned release", rel.Tag, need))
		}
	}

	sums, err := u.fetch(ctx, urls["checksums.txt"], maxSums)
	if err != nil {
		return res, err
	}
	sig, err := u.fetch(ctx, urls["checksums.txt.minisig"], maxSig)
	if err != nil {
		return res, err
	}
	comment, err := minisign.VerifyComment(key, sums, sig)
	if err != nil {
		return res, refuse(Unverified, fmt.Errorf("release %s's checksums.txt failed its signature check (%v); refusing it", rel.Tag, err))
	}
	if err := CheckSignedTag(comment, rel.Tag); err != nil {
		return res, refuse(Unverified, fmt.Errorf("release %s: %v; refusing it", rel.Tag, err))
	}
	want, err := checksumFor(sums, name)
	if err != nil {
		return res, refuse(Unverified, err)
	}

	archive, err := u.fetch(ctx, urls[name], maxArchive)
	if err != nil {
		return res, err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return res, refuse(Unverified, fmt.Errorf("%s does not match its signed checksum (a damaged or changed download); refusing it", name))
	}
	binary, err := extract(archive)
	if err != nil {
		return res, refuse(Failed, fmt.Errorf("%s: %v", name, err))
	}
	if err := u.swap(ctx, exe, start, binary, rel.Tag); err != nil {
		return res, err
	}
	res.Updated = true
	fmt.Fprintf(u.Out, "teamflow updated: %s -> %s (%s)\n", current, strings.TrimPrefix(rel.Tag, "v"), exe)
	if u.Replaced != nil {
		u.Replaced()
	}
	return res, nil
}

// takeTurn holds u.LockFile (flock), waiting up to u.LockWait for another update to finish; nothing to hold when
// there is no LockFile.
func (u *Updater) takeTurn(ctx context.Context) (func(), error) {
	if u.LockFile == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(u.LockFile), 0o700); err != nil {
		return nil, refuse(Failed, fmt.Errorf("update lock: %v", err))
	}
	f, err := os.OpenFile(u.LockFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, refuse(Failed, fmt.Errorf("update lock: %v", err))
	}
	wait := u.LockWait
	if wait <= 0 {
		wait = lockWait
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, refuse(Failed, fmt.Errorf("update lock: %v", err))
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			_ = f.Close()
			return nil, refuse(Failed, errors.New("another teamflow update is running; try again in a minute"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sweep removes temporary files of interrupted updates (older than staleTemp) from dir.
func sweep(dir string) {
	old, _ := filepath.Glob(filepath.Join(dir, ".teamflow-update-*"))
	for _, p := range old {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > staleTemp {
			_ = os.Remove(p)
		}
	}
}

// ExpectVersion is an Expect: the binary on disk must still say it is version (its --version), so an update never
// replaces a binary another install put there meanwhile.
func ExpectVersion(version string) func(exe string) error {
	return func(exe string) error {
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, exe, "--version").Output()
		got := strings.TrimSpace(string(out))
		if err != nil || got != "teamflow "+strings.TrimPrefix(version, "v") {
			return fmt.Errorf("the teamflow on disk is no longer this one (it says %q, not teamflow %s): another "+
				"install or update ran; it stays as it is", got, strings.TrimPrefix(version, "v"))
		}
		return nil
	}
}

// Latest is the newest release's tag, as GitHub names it (an *Error when it can't be asked).
func (u *Updater) Latest(ctx context.Context) (string, error) {
	rel, err := u.withClient().latest(ctx)
	return rel.Tag, err
}

// withClient is a copy of u whose client follows https redirects only (the caller's Updater and Client stay as
// they were).
func (u *Updater) withClient() *Updater {
	client := http.Client{Timeout: 5 * time.Minute}
	if u.Client != nil {
		client = *u.Client
	}
	client.CheckRedirect = httpsOnly
	c := *u
	c.Client = &client
	return &c
}

// swap writes binary into a temporary file beside exe, checks it runs and names tag, then renames it over exe
// (atomic in one folder), holding u.Hold meanwhile, if exe is still start (and what u.Expect says). Any failure
// leaves exe as it was and nothing beside it.
func (u *Updater) swap(ctx context.Context, exe string, start os.FileInfo, binary []byte, tag string) error {
	dir := filepath.Dir(exe)
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(exe); err == nil {
		mode = fi.Mode().Perm() | 0o100
	}
	tmp, err := os.CreateTemp(dir, ".teamflow-update-*")
	if err != nil {
		return refuse(NotWritable, fmt.Errorf("cannot write to %s: %v", dir, err))
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close()
		return refuse(Failed, fmt.Errorf("cannot write the new binary: %v", err))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return refuse(Failed, fmt.Errorf("cannot write the new binary: %v", err))
	}
	if err := tmp.Close(); err != nil {
		return refuse(Failed, fmt.Errorf("cannot write the new binary: %v", err))
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return refuse(Failed, fmt.Errorf("cannot write the new binary: %v", err))
	}
	if err := checkRuns(ctx, tmpName, tag); err != nil {
		return refuse(Failed, err)
	}
	if u.Hold != nil {
		release, err := u.Hold(ctx)
		if err != nil {
			return refuse(Failed, fmt.Errorf("the new binary was not put in place: %v", err))
		}
		defer release()
	}
	if now, err := os.Stat(exe); err != nil || !os.SameFile(start, now) {
		return refuse(Changed, fmt.Errorf("%s was replaced while this update ran (another install?); it stays as it is", exe))
	}
	if u.Expect != nil {
		if err := u.Expect(exe); err != nil {
			return refuse(Changed, err)
		}
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return refuse(Failed, fmt.Errorf("cannot replace %s: %v", exe, err))
	}
	keep = true
	return nil
}

// latest asks GitHub for the newest release.
func (u *Updater) latest(ctx context.Context) (ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.API, "/")+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return ghRelease{}, refuse(Failed, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "teamflow-self-update")
	resp, err := u.Client.Do(req)
	if err != nil {
		return ghRelease{}, refuse(Offline, fmt.Errorf("cannot reach GitHub to find the latest release (offline?): %v", unwrapURL(err)))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		when := "later"
		if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			when = "after " + time.Unix(s, 0).Format(time.Kitchen)
		}
		return ghRelease{}, refuse(RateLimited, fmt.Errorf("GitHub's API rate limit is reached for this network; try again %s, or download the new release from %s", when, ReleasesPage))
	case resp.StatusCode == http.StatusNotFound:
		return ghRelease{}, refuse(Failed, errors.New("no release is published yet"))
	case resp.StatusCode != http.StatusOK:
		return ghRelease{}, refuse(Failed, fmt.Errorf("GitHub answered %s when asked for the latest release; try again later", resp.Status))
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPI)).Decode(&rel); err != nil || rel.Tag == "" {
		return ghRelease{}, refuse(Failed, errors.New("GitHub's answer about the latest release could not be read; try again later"))
	}
	return rel, nil
}

// fetch downloads one release file over https, at most limit bytes.
func (u *Updater) fetch(ctx context.Context, link string, limit int64) ([]byte, error) {
	name := filepath.Base(link)
	if p, err := url.Parse(link); err != nil || p.Scheme != "https" {
		return nil, refuse(Unverified, fmt.Errorf("the link for %s is not https; refusing it", name))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, refuse(Failed, err)
	}
	req.Header.Set("User-Agent", "teamflow-self-update")
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, refuse(Offline, fmt.Errorf("download of %s failed: %v", name, unwrapURL(err)))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, refuse(Failed, fmt.Errorf("download of %s failed: %s", name, resp.Status))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, refuse(Offline, fmt.Errorf("download of %s failed: %v", name, err))
	}
	if int64(len(b)) > limit {
		return nil, refuse(Failed, fmt.Errorf("download of %s is larger than %d bytes; refusing it", name, limit))
	}
	return b, nil
}

// SignedTag is the release tag a signature's trusted comment names (its one tab-separated "tag:" field; the
// release workflow writes it, so a signature made for one release is not taken for another).
func SignedTag(comment string) (string, error) {
	tag, n := "", 0
	for _, f := range strings.Split(comment, "\t") {
		if v, ok := strings.CutPrefix(f, "tag:"); ok {
			tag, n = v, n+1
		}
	}
	switch n {
	case 0:
		return "", errors.New("the signature names no release tag")
	case 1:
		return tag, nil
	}
	return "", errors.New("the signature names more than one release tag")
}

// CheckSignedTag checks a trusted comment names exactly tag.
func CheckSignedTag(comment, tag string) error {
	got, err := SignedTag(comment)
	if err != nil {
		return err
	}
	if got != tag {
		return fmt.Errorf("the signature was signed for %s, not %s", got, tag)
	}
	return nil
}

// checksumFor finds name's SHA-256 in a checksums.txt ("<hex>  <name>" lines).
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("the signed checksums.txt has no checksum for %s; refusing it", name)
}

// extract takes the teamflow binary out of a release archive.
func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.New("not a gzip archive")
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("the archive holds no teamflow binary")
		}
		if err != nil {
			return nil, fmt.Errorf("the archive is damaged: %v", err)
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != "teamflow" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return nil, fmt.Errorf("the archive is damaged: %v", err)
		}
		if len(b) > maxBinary {
			return nil, errors.New("the binary in the archive is too large")
		}
		return b, nil
	}
}

// checkRuns runs the new binary's --version (it is verified by now): it must run on this machine and name tag.
func checkRuns(ctx context.Context, path, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return fmt.Errorf("the new binary does not run on this machine (%v); refusing it", err)
	}
	want := "teamflow " + strings.TrimPrefix(tag, "v")
	if strings.TrimSpace(string(out)) != want {
		return fmt.Errorf("the new binary says %q, not %s; refusing it", strings.TrimSpace(string(out)), tag)
	}
	return nil
}

// writable checks a new file can be made in dir (and removes it).
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".teamflow-update-*")
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return pe.Err
		}
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// Newer says whether tag is a release newer than current (see newer).
func Newer(tag, current string) bool { return newer(tag, current) }

// IsRelease says whether v is a release version (v1.2.3 or 1.2.3), not a development build (dev, a commit).
func IsRelease(v string) bool {
	_, ok := semver(v)
	return ok
}

// newer says whether tag is a newer release than current. A tag that is not a release version (v0.2.0-rc1) never
// is; a build that is not a release version (dev, a commit) takes any release; the same or an older one is never
// newer (no downgrade).
func newer(tag, current string) bool {
	t, ok1 := semver(tag)
	c, ok2 := semver(current)
	switch {
	case !ok1:
		return false
	case !ok2:
		return true
	}
	for i := range t {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

func semver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// httpsOnly refuses a redirect to anything but https (GitHub sends downloads to its storage host over https).
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("a redirect to %s is not https; refusing it", req.URL.Host)
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

// unwrapURL drops net/url's "Get <url>:" prefix.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
