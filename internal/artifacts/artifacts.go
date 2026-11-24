// Package artifacts is artifacts on the member's machine (the Go port of tf_client.artifacts and the file side
// of the artifact tools and commands): reading a file to publish, its kind and media type, its digest, whether
// it reads as text, and pulling a stored version back.
//
// The server stores any type of file up to MaxBytes. Anything larger stays where the team can reach it and is
// published as a link, with the size, media type and SHA-256 computed here: it is refused before anything is
// sent, with the hint to publish a link. Teamflow uploads nothing big.
package artifacts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// MaxBytes is the most the server stores for one artifact version (MAX_ARTIFACT_BYTES).
const MaxBytes = protocol.MaxArtifactBytes

// InlineChars is how many characters artifact_get returns inline; more is saved to a file.
const InlineChars = 100_000

type media struct {
	media string
	kind  protocol.ArtifactKind
}

// extensions: by extension, (media type, kind).
var extensions = func() map[string]media {
	m := map[string]media{}
	add := func(mt string, k protocol.ArtifactKind, exts ...string) {
		for _, e := range exts {
			m[e] = media{mt, k}
		}
	}
	add("text/markdown", protocol.ArtifactKindText, ".md", ".markdown")
	add("text/plain", protocol.ArtifactKindText, ".txt")
	add("text/html", protocol.ArtifactKindText, ".html", ".htm")
	add("text/csv", protocol.ArtifactKindData, ".csv")
	add("text/tab-separated-values", protocol.ArtifactKindData, ".tsv")
	add("application/json", protocol.ArtifactKindData, ".json")
	add("application/yaml", protocol.ArtifactKindData, ".yaml", ".yml")
	add("text/x-python", protocol.ArtifactKindCode, ".py")
	add("text/javascript", protocol.ArtifactKindCode, ".js", ".mjs", ".jsx")
	add("text/x-typescript", protocol.ArtifactKindCode, ".ts", ".tsx")
	add("text/plain", protocol.ArtifactKindCode, ".go", ".rs", ".java", ".kt", ".swift", ".c", ".h", ".cpp", ".rb",
		".sh", ".sql", ".css", ".scss", ".toml")
	add("application/pdf", protocol.ArtifactKindDocument, ".pdf")
	add("application/vnd.openxmlformats-officedocument.wordprocessingml.document", protocol.ArtifactKindDocument, ".docx")
	add("application/vnd.openxmlformats-officedocument.presentationml.presentation", protocol.ArtifactKindPresentation, ".pptx")
	add("application/vnd.apple.keynote", protocol.ArtifactKindPresentation, ".key")
	add("application/vnd.oasis.opendocument.presentation", protocol.ArtifactKindPresentation, ".odp")
	return m
}()

// byMedia: by media type, for a file whose extension is not listed (the first kind listed for a type wins:
// text/plain is TEXT, from .txt, before the code files).
var byMedia = map[string]protocol.ArtifactKind{
	"text/markdown": protocol.ArtifactKindText, "text/plain": protocol.ArtifactKindText,
	"text/html": protocol.ArtifactKindText, "text/csv": protocol.ArtifactKindData,
	"text/tab-separated-values": protocol.ArtifactKindData, "application/json": protocol.ArtifactKindData,
	"application/yaml": protocol.ArtifactKindData, "text/x-python": protocol.ArtifactKindCode,
	"text/javascript": protocol.ArtifactKindCode, "text/x-typescript": protocol.ArtifactKindCode,
	"application/pdf": protocol.ArtifactKindDocument,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   protocol.ArtifactKindDocument,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": protocol.ArtifactKindPresentation,
	"application/vnd.apple.keynote":                                             protocol.ArtifactKindPresentation,
	"application/vnd.oasis.opendocument.presentation":                           protocol.ArtifactKindPresentation,
}

// topLevel: by the media type's top level, when nothing else says.
var topLevel = map[string]protocol.ArtifactKind{"image": protocol.ArtifactKindImage, "audio": protocol.ArtifactKindAudio,
	"video": protocol.ArtifactKindVideo, "text": protocol.ArtifactKindText}

// textBased are the kinds whose content is text (TEXT_BASED).
func textBased(k protocol.ArtifactKind) bool {
	return k == protocol.ArtifactKindText || k == protocol.ArtifactKindData || k == protocol.ArtifactKindCode
}

// modern are media types newer Pythons (and most machines' system tables) know but Python 3.12's built-in
// table does not: without them a .webp would be a DOCUMENT.
var modern = map[string]string{".webp": "image/webp", ".avif": "image/avif", ".heic": "image/heic",
	".webm": "video/webm", ".mkv": "video/x-matroska", ".opus": "audio/opus", ".flac": "audio/flac",
	".m4a": "audio/mp4"}

// IsText says whether content of this media type is text, whatever kind its writer gave it.
func IsText(mediaType string) bool {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	if strings.HasPrefix(m, "text/") {
		return true
	}
	k, ok := byMedia[m]
	return ok && textBased(k)
}

// splitext is Python's posixpath.splitext: leading dots do not start an extension.
func splitext(p string) (string, string) {
	sep := strings.LastIndex(p, "/")
	dot := strings.LastIndex(p, ".")
	if dot > sep {
		i := sep + 1
		for i < dot && p[i] == '.' {
			i++
		}
		if i < dot {
			return p[:dot], p[dot:]
		}
	}
	return p, ""
}

// suffix is pathlib's Path(name).suffix: the last part's extension ("" for ".bashrc" and "a.").
func suffix(name string) string {
	base := path.Base(name)
	i := strings.LastIndex(base, ".")
	if i <= 0 || i == len(base)-1 || strings.Trim(base, ".") == "" {
		return ""
	}
	return base[i:]
}

// guessType is Python's mimetypes.guess_type on its built-in tables.
func guessType(name string) string {
	base, ext := splitext(name)
	for {
		s, ok := pySuffixes[strings.ToLower(ext)]
		if !ok {
			break
		}
		base, ext = splitext(base + s)
	}
	if _, ok := pyEncodings[ext]; ok { // case sensitive
		base, ext = splitext(base)
	}
	ext = strings.ToLower(ext)
	if t, ok := pyTypes[ext]; ok {
		return t
	}
	return modern[ext]
}

// Infer is the kind and media type of a file: from what was given, its extension, then its media type.
func Infer(name, mediaType string, kind protocol.ArtifactKind) (protocol.ArtifactKind, string) {
	known, isKnown := extensions[strings.ToLower(suffix(name))]
	m := mediaType
	if m == "" && isKnown {
		m = known.media
	}
	if m == "" {
		m = guessType(name)
	}
	if m == "" {
		m = "application/octet-stream"
	}
	m = strings.ToLower(m)
	if kind == "" {
		switch k, ok := byMedia[m]; {
		case isKnown:
			kind = known.kind
		case ok:
			kind = k
		default:
			kind = protocol.ArtifactKindDocument
			if k, ok := topLevel[strings.SplitN(m, "/", 2)[0]]; ok {
				kind = k
			}
		}
	}
	return kind, m
}

// Digest is the SHA-256 of data, in hex.
func Digest(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// SizeText is a size for people: "24 B", "12 KB", "5.0 MB"; nil is "size unknown".
func SizeText(n *int64) string {
	switch {
	case n == nil:
		return "size unknown"
	case *n < 1024:
		return fmt.Sprintf("%d B", *n)
	case *n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(*n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(*n)/1024/1024)
}

// Readable is the content as text, when it is to be read inline: a text-based kind or a text media type, valid
// UTF-8, and at most InlineChars characters. Otherwise it is to be saved to a file.
func Readable(data []byte, kind protocol.ArtifactKind, mediaType string) (string, bool) {
	if !textBased(kind) && !IsText(mediaType) {
		return "", false
	}
	if !utf8.Valid(data) || utf8.RuneCount(data) > InlineChars {
		return "", false
	}
	return string(data), true
}

// Prepared is a file read to publish: what the artifact_write call needs.
type Prepared struct {
	Name      string
	Kind      protocol.ArtifactKind
	MediaType string
	Size      int64
	SHA256    string
	Text      bool   // its content reads as text (a text kind or media type, and valid UTF-8)
	Data      []byte // nil when the file is over MaxBytes
}

// TooLargeError is a file over MaxBytes: nothing is sent; it stays where the team can reach it and is published
// as a link. The error is the server's own sentence for it.
type TooLargeError struct{ Prepared }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%.1f MB is over the 5 MB limit: keep it where your team can reach it and publish it as a link",
		float64(e.Size)/1024/1024)
}

// TooLargeNext is what the artifact_write tool tells the agent to do with a file over the limit.
const TooLargeNext = "Teamflow stores files up to 5 MB. Ask the person where this file should live for the team " +
	"(somewhere teammates already have access to), then call artifact_publish with its url, name, topic, kind, " +
	"summary, points, size, sha256 and media_type as given here."

// Answer is the artifact_write tool's answer for a file over the limit (the Python tool's, field for field).
func (e *TooLargeError) Answer() map[string]any {
	return map[string]any{"accepted": false, "reason": string(protocol.RejectReasonArtifactTooLarge),
		"message": e.Error(), "name": e.Name, "kind": string(e.Kind), "size": e.Size, "sha256": e.SHA256,
		"media_type": e.MediaType, "next": TooLargeNext}
}

// LinkHint is what `teamflow artifact write` prints for a file over the limit: the command that publishes it
// as a link once it is where the team can reach it.
func LinkHint(size int64, name, topic, file string) string {
	return fmt.Sprintf("%s is over the 5 MB limit. Keep it where your team can reach it, then: teamflow artifact "+
		"link <url> --name %s --topic %s --summary ... --file %s", SizeText(&size), shellQuote(name),
		shellQuote(topic), shellQuote(file))
}

// shellQuote is Python's shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_@%+=:,./-", r))) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Options are what Prepare may be told: the artifact's name (default: the file's), its media type and kind
// (default: inferred), the session's project folder and the Teamflow home.
type Options struct {
	Name      string
	MediaType string
	Kind      protocol.ArtifactKind
	// Project is the session's project folder. A path inside it must stay inside it once links are followed: a
	// link in a checked-out repo (docs/spec.md -> ~/.ssh/id_ed25519) is never read. A path named outside the
	// project directly is the person's or agent's own choice, as in the Python client.
	Project string
	// Home is the Teamflow home (default: home.Dir()). Nothing in it is ever published: it holds the keys.
	Home string
}

// under says whether p is the folder dir or inside it, by identity: each of p and its parents is compared with
// dir as a file (os.SameFile), so a case variant on a case-insensitive file system, or a path through a link to
// dir, is still dir.
func under(p string, dir os.FileInfo) bool {
	if dir == nil {
		return false
	}
	for d := filepath.Clean(p); ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && os.SameFile(fi, dir) {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

func statDir(p string) os.FileInfo {
	if p == "" {
		return nil
	}
	fi, err := os.Stat(expand(p))
	if err != nil || !fi.IsDir() {
		return nil
	}
	return fi
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// InHome says whether path (as given, ~ expanded) is the Teamflow home (tfHome; "": home.Dir()) or inside it,
// by identity, with links followed: directly, through .., through a link to a file in it or to the home, or as a
// case variant on a disk that ignores case. A path that does not exist yet is judged by its nearest existing
// folder (so a file about to be written into the home counts).
func InHome(p, tfHome string) bool {
	if tfHome == "" {
		tfHome = home.Dir()
	}
	h := statDir(tfHome)
	if h == nil {
		return false
	}
	abs, err := filepath.Abs(expand(p))
	if err != nil {
		return false
	}
	if under(abs, h) {
		return true
	}
	for d := abs; ; d = filepath.Dir(d) {
		if real, err := filepath.EvalSymlinks(d); err == nil {
			return under(real, h)
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// HomeRefusal is the refusal for a path in the Teamflow home.
func HomeRefusal(p string) error {
	return fmt.Errorf("%s is in Teamflow's home, among Teamflow's own files (your sign-in and keys): no command reads or writes a file there", home.Display(p))
}

// MaxTextFile is the most ReadText reads (a plan, an instruction, a Deliverables file).
const MaxTextFile = 8 << 20

// ReadText reads a text file a command was given (--plan-file and the like): never one in the Teamflow home
// (InHome), only a regular file, and the file read is the file checked.
func ReadText(p string) ([]byte, error) {
	f, _, shown, err := open(p, "", "")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxTextFile+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", shown, err)
	}
	if len(b) > MaxTextFile {
		return nil, fmt.Errorf("%s is over %d MB", shown, MaxTextFile>>20)
	}
	return b, nil
}

// open opens a file to read for publishing or measuring, after every check: never a file in the Teamflow home,
// never a link from inside the project to outside it (when project is set), only a regular file (checked before
// opening: a FIFO would block), and the file opened must be the file checked.
func open(p, project, tfHome string) (*os.File, string, string, error) {
	if p == "" {
		return nil, "", "", errors.New("no file given")
	}
	abs, err := filepath.Abs(expand(p))
	if err != nil {
		return nil, "", "", fmt.Errorf("no file %s: %w", home.Display(p), err)
	}
	shown := home.Display(abs)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", "", fmt.Errorf("no file %s", shown)
	}
	if tfHome == "" {
		tfHome = home.Dir()
	}
	if h := statDir(tfHome); under(real, h) || under(abs, h) {
		return nil, "", "", HomeRefusal(abs)
	}
	if pr := statDir(project); under(abs, pr) && !under(real, pr) {
		return nil, "", "", fmt.Errorf("%s is a link to a file outside the project: publish the file itself, "+
			"from where it is", shown)
	}
	checked, err := os.Stat(real)
	if err != nil {
		return nil, "", "", fmt.Errorf("no file %s", shown)
	}
	if checked.IsDir() {
		return nil, "", "", fmt.Errorf("%s is a folder: publish a file", shown)
	}
	if !checked.Mode().IsRegular() {
		return nil, "", "", fmt.Errorf("%s is not a regular file", shown)
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", "", fmt.Errorf("cannot read %s: %w", shown, errors.Unwrap(err))
	}
	if fi, err := f.Stat(); err != nil || !os.SameFile(fi, checked) {
		f.Close()
		return nil, "", "", fmt.Errorf("%s changed while it was being read: try again", shown)
	}
	return f, abs, shown, nil
}

// Prepare reads a file to publish. Over MaxBytes, it returns the file's size, digest, kind and media type with
// a *TooLargeError and no bytes. A path inside the project that leads out of it, any path into the Teamflow
// home, a folder, anything but a regular file, or a missing file is refused.
func Prepare(p string, o Options) (Prepared, error) {
	f, abs, shown, err := open(p, o.Project, o.Home)
	if err != nil {
		return Prepared{}, err
	}
	defer f.Close()
	name := o.Name
	if name == "" {
		name = filepath.Base(abs)
	}
	kind, mt := Infer(name, o.MediaType, o.Kind)
	pr := Prepared{Name: name, Kind: kind, MediaType: mt}
	// Read at most one byte past the limit; past it, only hash the rest.
	h := sha256.New()
	data, err := io.ReadAll(io.TeeReader(io.LimitReader(f, MaxBytes+1), h))
	if err != nil {
		return Prepared{}, fmt.Errorf("cannot read %s: %w", shown, err)
	}
	if len(data) > MaxBytes {
		rest, err := io.Copy(h, f)
		if err != nil {
			return Prepared{}, fmt.Errorf("cannot read %s: %w", shown, err)
		}
		pr.Size, pr.SHA256 = int64(len(data))+rest, hex.EncodeToString(h.Sum(nil))
		return pr, &TooLargeError{pr}
	}
	return withData(pr, data), nil
}

func withData(p Prepared, data []byte) Prepared {
	p.Data, p.Size, p.SHA256 = data, int64(len(data)), Digest(data)
	_, p.Text = Readable(data, p.Kind, p.MediaType)
	if !p.Text && (textBased(p.Kind) || IsText(p.MediaType)) && utf8.Valid(data) {
		p.Text = true // text, just longer than is shown inline
	}
	return p
}

// FromContent is text given in place of a file (the artifact_write tool's content): it needs a name, and a name
// with no known type is plain text.
func FromContent(content, name, mediaType string, kind protocol.ArtifactKind) (Prepared, error) {
	if name == "" {
		return Prepared{}, errors.New("name the artifact (a file name such as notes.md)")
	}
	if mediaType == "" {
		if _, m := Infer(name, "", ""); m == "application/octet-stream" {
			mediaType = "text/plain"
		}
	}
	k, m := Infer(name, mediaType, kind)
	p := withData(Prepared{Name: name, Kind: k, MediaType: m}, []byte(content))
	if p.Size > MaxBytes {
		p.Data = nil
		return p, &TooLargeError{p}
	}
	return p, nil
}

// Content is where an artifact version's content is, for a link: its url, and what a local copy says of it.
type Content struct {
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Size      *int64 `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// Signal is an ARTIFACT signal as the agent or person describes it (Python's artifact_signal): title defaults
// to the name (at most 120 characters), and approvers, when given, must all approve.
func Signal(name, topic string, kind protocol.ArtifactKind, summary string, points []string, title string,
	supersedes *string, approvers []string, c Content) map[string]any {
	if points == nil {
		points = []string{}
	}
	if title == "" {
		title = name
	}
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120])
	}
	body := map[string]any{"type": "artifact.v1", "name": name, "kind": string(kind), "summary": summary, "points": points}
	if c.URL != "" {
		body["url"] = c.URL
	}
	if c.MediaType != "" {
		body["media_type"] = c.MediaType
	}
	if c.Size != nil {
		body["size"] = *c.Size
	}
	if c.SHA256 != "" {
		body["sha256"] = c.SHA256
	}
	sig := map[string]any{"kind": string(protocol.SignalKindArtifact), "topic": topic, "title": title,
		"supersedes": supersedes, "body": body}
	if len(approvers) > 0 {
		sig["request_for_approval"] = map[string]any{"approvers": approvers, "required": len(approvers)}
	}
	return sig
}

// Link is an artifact that lives elsewhere (`teamflow artifact link`): its kind and media type from the name,
// and, from a local copy when one is given, its size and SHA-256. The copy is only measured, never sent.
func Link(url, name, file string, kind protocol.ArtifactKind) (Content, protocol.ArtifactKind, error) {
	k, m := Infer(name, "", kind)
	c := Content{URL: url, MediaType: m}
	if file == "" {
		return c, k, nil
	}
	f, _, shown, err := open(file, "", "")
	if err != nil {
		return Content{}, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Content{}, "", fmt.Errorf("cannot read %s: %w", shown, err)
	}
	c.Size, c.SHA256 = &n, hex.EncodeToString(h.Sum(nil))
	return c, k, nil
}

// pullRace lets a test change the file system between the checks and the open (a race an agent could run).
var pullRace = func(stage string) {}

// writeOutsideHome writes data to target, never into the Teamflow home: the final target is checked (InHome) as
// given, again after its folders are made, and on the opened file by identity (a folder swapped for a link to the
// home between check and open, or a hard link to one of its files, is refused before anything is truncated). A
// link is never written through; an existing file is replaced only when replace (the person named it).
func writeOutsideHome(target string, data []byte, replace bool) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if InHome(abs, "") {
		return HomeRefusal(target)
	}
	existed := false
	if fi, err := os.Lstat(abs); err == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is a link: not written through; pass -o <file>", target)
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s is not a file", target)
		case !replace:
			return fmt.Errorf("%s already exists: not replaced; pass -o <file>", target)
		}
		existed = true
	}
	if replace {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
	}
	if InHome(abs, "") { // a folder swapped for a link while it was made
		return HomeRefusal(target)
	}
	pullRace("open")
	// O_NONBLOCK: a FIFO swapped in never makes the open wait. No file there: O_CREATE|O_EXCL, so a file made here
	// is known to be this call's; over an existing file no O_CREATE, so one removed meanwhile is never made anew.
	flags := os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if !existed {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(abs, flags, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists: not replaced; pass -o <file>", target)
	}
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", target, errors.Unwrap(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	// Checked on the opened file, before anything is truncated.
	if !fi.Mode().IsRegular() || homeFile(fi, home.Dir()) || InHome(abs, "") {
		if !existed { // made by this call (O_EXCL): never left behind, not even empty
			if now, err := os.Lstat(abs); err == nil && os.SameFile(now, fi) {
				_ = os.Remove(abs)
			}
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a file", target)
		}
		return HomeRefusal(target)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}

// homeFile says whether fi is one of the files in the Teamflow home (by identity: a hard link to it is it).
func homeFile(fi os.FileInfo, dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if hfi, err := os.Stat(p); err == nil && os.SameFile(hfi, fi) {
			found = true
		}
		return nil
	})
	return found
}

// Pulled is what Pull did: the file written, or, for a link, its url.
type Pulled struct {
	Path, URL, Version string
	ChainVersion       int64
}

// Line is what `teamflow artifact pull` prints.
func (p Pulled) Line() string {
	switch {
	case p.Path != "":
		return fmt.Sprintf("%s\t%s (v%d)", p.Path, p.Version, p.ChainVersion)
	case p.URL != "":
		return p.URL
	}
	return "no stored content"
}

// Pull saves an artifact version's stored content to output, or to its name in the current folder; a link is
// given back as its url. The name is a teammate's: only its last part is used, so it never picks a folder.
// Content that does not match its stored SHA-256, or has none, is refused, and nothing is written. Without
// output an existing file is never replaced; a link is never written through.
func Pull(c protocol.ArtifactContent, output string) (Pulled, error) {
	body, ok := c.Signal.Signal.Body.(protocol.ArtifactBody)
	if !ok {
		return Pulled{}, fmt.Errorf("%s is not an artifact", c.Signal.Signal.ID)
	}
	out := Pulled{Version: c.Signal.Signal.ID, ChainVersion: c.Signal.ChainVersion}
	if c.ContentB64 == nil {
		if body.URL != nil {
			out.URL = *body.URL
		}
		return out, nil
	}
	data, err := base64.StdEncoding.DecodeString(*c.ContentB64)
	if err != nil {
		return Pulled{}, fmt.Errorf("the server's content for %s is not base64", out.Version)
	}
	if body.Stored == nil || body.Stored.Sha256 == "" {
		return Pulled{}, fmt.Errorf("%s has no SHA-256 to check its content against: not saved", out.Version)
	}
	if !strings.EqualFold(body.Stored.Sha256, Digest(data)) {
		return Pulled{}, fmt.Errorf("the content of %s does not match its SHA-256: not saved", out.Version)
	}
	// Without -o the name is a teammate's: a new file only, never an existing one or a link (it could pick
	// .envrc or a Makefile). With -o the person chose the file: it may be replaced, but not written through a link.
	target := expand(output)
	if target == "" {
		target = filepath.Base(body.Name)
		if target == "." || target == ".." || target == "/" || body.Name == "" {
			return Pulled{}, fmt.Errorf("the artifact's name %q is not a file name: pass -o <file>", body.Name)
		}
	}
	if err := writeOutsideHome(target, data, output != ""); err != nil {
		return Pulled{}, err
	}
	out.Path = target
	return out, nil
}
