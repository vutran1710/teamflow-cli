package artifacts

// Python twins (tests/client/test_artifacts.py):
//
//	test_agent_writes_reads_and_versions_an_artifact                 → TestTextDetection (pricing-spec.md inline)
//	test_a_text_file_comes_inline_whatever_kind_the_writer_gave       → TestTextDetection (a Markdown DOCUMENT)
//	test_media_saved_to_a_file_and_large_files_by_link                → TestTextDetection (png), TestOverLimitRefusedWithLinkHint
//	test_cli_write_pull_archive_list (pull part)                      → TestPull

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

const mb = 1024 * 1024

func write(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// real is a temp dir with its symlinks resolved (macOS's /var is a link to /private/var).
func real(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOverLimitRefusedWithLinkHint(t *testing.T) {
	dir := real(t)
	big := write(t, filepath.Join(dir, "talk.mp4"), bytes.Repeat([]byte{0}, MaxBytes+1))
	p, err := Prepare(big, Options{})
	var tl *TooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("err %v, want a TooLargeError", err)
	}
	if p.Data != nil || tl.Data != nil {
		t.Fatal("the bytes of a file over the limit are kept")
	}
	want := sum(bytes.Repeat([]byte{0}, MaxBytes+1))
	if p.Size != MaxBytes+1 || p.SHA256 != want || p.Kind != protocol.ArtifactKindVideo || p.MediaType != "video/mp4" ||
		p.Name != "talk.mp4" {
		t.Fatalf("prepared %+v", p)
	}
	if err.Error() != "5.0 MB is over the 5 MB limit: keep it where your team can reach it and publish it as a link" {
		t.Fatalf("message %q", err)
	}
	a := tl.Answer()
	b, _ := json.Marshal(a)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["accepted"] != false || got["reason"] != "ARTIFACT_TOO_LARGE" || got["kind"] != "VIDEO" ||
		got["size"] != float64(MaxBytes+1) || got["sha256"] != want || got["media_type"] != "video/mp4" ||
		got["name"] != "talk.mp4" || got["message"] != err.Error() {
		t.Fatalf("answer %s", b)
	}
	next, _ := got["next"].(string)
	if !strings.Contains(next, "artifact_publish") || !strings.Contains(next, "5 MB") || strings.Contains(next, "upload_to") {
		t.Fatalf("next %q", next)
	}
	hint := LinkHint(p.Size, "talk.mp4", "Pricing talk", "/work/it's here/talk.mp4")
	if hint != `5.0 MB is over the 5 MB limit. Keep it where your team can reach it, then: teamflow artifact link <url> `+
		`--name talk.mp4 --topic 'Pricing talk' --summary ... --file '/work/it'"'"'s here/talk.mp4'` {
		t.Fatalf("hint %q", hint)
	}

	// exactly at the limit: kept, with its bytes
	at := write(t, filepath.Join(dir, "at.bin"), bytes.Repeat([]byte{1}, MaxBytes))
	p, err = Prepare(at, Options{})
	if err != nil || len(p.Data) != MaxBytes || p.Size != MaxBytes {
		t.Fatalf("at the limit: %v, %d bytes", err, len(p.Data))
	}

	// content given as text is held to the same limit
	_, err = FromContent(strings.Repeat("x", MaxBytes+1), "notes.md", "", "")
	if !errors.As(err, &tl) || tl.Kind != protocol.ArtifactKindText {
		t.Fatalf("content over the limit: %v", err)
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 24: "24 B", 1023: "1023 B", 1024: "1 KB", 1536: "2 KB",
		2560: "2 KB", mb - 1: "1024 KB", mb: "1.0 MB", 5*mb + 1: "5.0 MB", 5*mb + mb/2: "5.5 MB"} {
		if got := SizeText(&n); got != want {
			t.Errorf("SizeText(%d) = %q, want %q", n, got, want)
		}
	}
	if SizeText(nil) != "size unknown" {
		t.Error("nil size")
	}
}

func TestInfer(t *testing.T) {
	cases := []struct {
		name, media string
		kind        protocol.ArtifactKind
		wantKind    protocol.ArtifactKind
		wantMedia   string
	}{
		{"talk.mp4", "", "", protocol.ArtifactKindVideo, "video/mp4"},
		{"chart.png", "", "", protocol.ArtifactKindImage, "image/png"},
		{"report.pdf", "", "", protocol.ArtifactKindDocument, "application/pdf"},
		{"notes.MD", "", "", protocol.ArtifactKindText, "text/markdown"},
		{"x.tar.gz", "", "", protocol.ArtifactKindDocument, "application/x-tar"},
		{".bashrc", "", "", protocol.ArtifactKindDocument, "application/octet-stream"},
		{"page", "Text/HTML", "", protocol.ArtifactKindText, "text/html"},
		{"spec.md", "", protocol.ArtifactKindDocument, protocol.ArtifactKindDocument, "text/markdown"},
		{"song.mp3", "", "", protocol.ArtifactKindAudio, "audio/mpeg"},
		{"data.tsv", "", "", protocol.ArtifactKindData, "text/tab-separated-values"},
		{"a.zzz", "", "", protocol.ArtifactKindDocument, "application/octet-stream"},
		{"main.go", "", "", protocol.ArtifactKindCode, "text/plain"},
		{"photo.JPG", "", "", protocol.ArtifactKindImage, "image/jpeg"},
		{"x.svgz", "", "", protocol.ArtifactKindImage, "image/svg+xml"},
		{"deck.key", "", "", protocol.ArtifactKindPresentation, "application/vnd.apple.keynote"},
		{"dir/file.yml", "", "", protocol.ArtifactKindData, "application/yaml"},
		{"noext", "", "", protocol.ArtifactKindDocument, "application/octet-stream"},
		{"clip.mov", "", "", protocol.ArtifactKindVideo, "video/quicktime"},
		{"a.wav", "", "", protocol.ArtifactKindAudio, "audio/x-wav"},
		{"a.webp", "", "", protocol.ArtifactKindImage, "image/webp"},
		{"a.", "", "", protocol.ArtifactKindDocument, "application/octet-stream"},
		{"a.GZ", "", "", protocol.ArtifactKindDocument, "application/octet-stream"}, // encodings are case sensitive
		{"a.json.gz", "", "", protocol.ArtifactKindData, "application/json"},
	}
	for _, c := range cases {
		k, m := Infer(c.name, c.media, c.kind)
		if k != c.wantKind || m != c.wantMedia {
			t.Errorf("Infer(%q, %q, %q) = %s %s, want %s %s", c.name, c.media, c.kind, k, m, c.wantKind, c.wantMedia)
		}
	}
}

func TestTextDetection(t *testing.T) {
	png := append([]byte("\x89PNG"), func() []byte {
		b := make([]byte, 256)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}()...)
	cases := []struct {
		what   string
		data   []byte
		kind   protocol.ArtifactKind
		media  string
		inline bool
	}{
		{"a Markdown spec", []byte("# Rounding\nSECRET-BYTES\n"), protocol.ArtifactKindText, "text/markdown", true},
		{"Markdown its writer called a DOCUMENT", []byte("# Pricing rules\n- 0.05\n"), protocol.ArtifactKindDocument, "text/markdown", true},
		{"a PNG", png, protocol.ArtifactKindImage, "image/png", false},
		{"text bytes in a media kind", []byte("hello"), protocol.ArtifactKindImage, "image/png", false},
		{"JSON data", []byte(`{"a": 1}`), protocol.ArtifactKindData, "application/json", true},
		{"YAML as a document", []byte("a: 1\n"), protocol.ArtifactKindDocument, "application/yaml", true},
		{"code", []byte("print('hi')\n"), protocol.ArtifactKindCode, "text/x-python", true},
		{"text media with parameters", []byte("x"), protocol.ArtifactKindDocument, "Text/Plain; charset=utf-8", true},
		{"invalid UTF-8 in a text file", []byte("caf\xe9\n"), protocol.ArtifactKindText, "text/plain", false},
		{"a PDF", []byte("%PDF-1.7\n"), protocol.ArtifactKindDocument, "application/pdf", false},
		{"empty text", []byte{}, protocol.ArtifactKindText, "text/plain", true},
		{"100,000 characters (multibyte)", []byte(strings.Repeat("é", InlineChars)), protocol.ArtifactKindText, "text/plain", true},
		{"100,001 characters", []byte(strings.Repeat("a", InlineChars+1)), protocol.ArtifactKindText, "text/plain", false},
		{"an unknown type", []byte("text"), protocol.ArtifactKindDocument, "application/octet-stream", false},
	}
	for _, c := range cases {
		text, ok := Readable(c.data, c.kind, c.media)
		if ok != c.inline || (ok && text != string(c.data)) {
			t.Errorf("%s: inline %v, want %v", c.what, ok, c.inline)
		}
	}
	for media, want := range map[string]bool{"text/csv": true, "application/json": true, "application/yaml": true,
		"text/x-python": true, "application/pdf": false, "image/svg+xml": false, "": false, " TEXT/html ; q=1": true} {
		if IsText(media) != want {
			t.Errorf("IsText(%q) = %v", media, !want)
		}
	}

	// From a file: Prepare says whether it is readable text.
	dir := real(t)
	p, err := Prepare(write(t, filepath.Join(dir, "pricing-spec.md"), []byte("# Rounding\n")), Options{Kind: protocol.ArtifactKindDocument})
	if err != nil || !p.Text || p.Kind != protocol.ArtifactKindDocument || p.MediaType != "text/markdown" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = Prepare(write(t, filepath.Join(dir, "chart.png"), png), Options{})
	if err != nil || p.Text || p.MediaType != "image/png" || p.SHA256 != sum(png) || !bytes.Equal(p.Data, png) {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestSymlinkOutsideProjectRefused(t *testing.T) {
	root := real(t)
	project := filepath.Join(root, "project")
	tfHome := filepath.Join(root, "dot-teamflow")
	outside := write(t, filepath.Join(root, "secrets", "id_key"), []byte("PRIVATE"))
	keys := write(t, filepath.Join(tfHome, "store.db"), []byte("KEYS"))
	inside := write(t, filepath.Join(project, "docs", "spec.md"), []byte("# Spec\n"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(outside, filepath.Join(project, "notes.md")))                 // a file link out of the project
	must(os.Symlink(filepath.Join(root, "secrets"), filepath.Join(project, "s"))) // a folder link out of it
	must(os.Symlink(inside, filepath.Join(project, "spec-link.md")))              // a link that stays inside
	must(os.Symlink("../secrets/id_key", filepath.Join(project, "rel.md")))       // a relative link out
	must(os.Symlink(keys, filepath.Join(root, "store-link")))                     // outside the project, into the home
	o := Options{Project: project, Home: tfHome}

	for _, path := range []string{filepath.Join(project, "notes.md"), filepath.Join(project, "s", "id_key"),
		filepath.Join(project, "rel.md"), filepath.Join(project, "docs", "..", "notes.md")} {
		p, err := Prepare(path, o)
		if err == nil || !strings.Contains(err.Error(), "outside the project") || p.Data != nil {
			t.Errorf("%s: %v (%d bytes read)", path, err, len(p.Data))
		}
		if err != nil && strings.Contains(err.Error(), "secrets") {
			t.Errorf("the refusal shows where the link leads: %v", err)
		}
	}
	for _, path := range []string{keys, filepath.Join(root, "store-link"), filepath.Join(tfHome, ".", "store.db")} {
		if _, err := Prepare(path, o); err == nil || !strings.Contains(err.Error(), "Teamflow's own") {
			t.Errorf("%s: %v", path, err)
		}
	}
	// A link that stays inside the project, and a file named outside it directly (the person's choice), are fine.
	for path, want := range map[string]string{filepath.Join(project, "spec-link.md"): "# Spec\n", outside: "PRIVATE"} {
		p, err := Prepare(path, o)
		if err != nil || string(p.Data) != want {
			t.Errorf("%s: %v %q", path, err, p.Data)
		}
	}
	if p, err := Prepare(filepath.Join(project, "spec-link.md"), o); err != nil || p.Name != "spec-link.md" {
		t.Errorf("the name is the link's own: %+v %v", p, err)
	}
	// A relative path is read from the current folder.
	t.Chdir(project)
	if p, err := Prepare("docs/spec.md", o); err != nil || string(p.Data) != "# Spec\n" {
		t.Errorf("relative: %v", err)
	}
	if _, err := Prepare("notes.md", o); err == nil {
		t.Error("a relative link out of the project was read")
	}
	// Sad paths: missing, a folder, a dangling link, an empty path.
	must(os.Symlink(filepath.Join(project, "gone"), filepath.Join(project, "dangling")))
	for path, want := range map[string]string{filepath.Join(project, "nope.md"): "no file", filepath.Join(project, "docs"): "a folder",
		filepath.Join(project, "dangling"): "no file", "": "no file"} {
		if _, err := Prepare(path, o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", path, err, want)
		}
	}
}

func TestFromContent(t *testing.T) {
	p, err := FromContent("# Notes\n", "notes.md", "", "")
	if err != nil || p.Kind != protocol.ArtifactKindText || p.MediaType != "text/markdown" || string(p.Data) != "# Notes\n" ||
		p.Size != 8 || p.SHA256 != sum([]byte("# Notes\n")) || !p.Text {
		t.Fatalf("%+v %v", p, err)
	}
	// text under a name with no known type is plain text, as the Python tool says
	if p, _ := FromContent("x", "README", "", ""); p.MediaType != "text/plain" || p.Kind != protocol.ArtifactKindText {
		t.Fatalf("%+v", p)
	}
	if p, _ := FromContent("x", "README", "application/x-thing", ""); p.MediaType != "application/x-thing" {
		t.Fatalf("a given media type is kept: %+v", p)
	}
	if _, err := FromContent("x", "", "", ""); err == nil || !strings.Contains(err.Error(), "name the artifact") {
		t.Fatalf("no name: %v", err)
	}
}

func TestSignal(t *testing.T) {
	size := int64(3)
	got := Signal("n.md", "Rounding", protocol.ArtifactKindText, "s", []string{"p"}, "", nil, []string{"mb_1"},
		Content{Size: &size})
	b, _ := json.Marshal(got)
	want := `{"body":{"kind":"TEXT","name":"n.md","points":["p"],"size":3,"summary":"s","type":"artifact.v1"},"kind":"ARTIFACT",` +
		`"request_for_approval":{"approvers":["mb_1"],"required":1},"supersedes":null,"title":"n.md","topic":"Rounding"}`
	if string(b) != want {
		t.Fatalf("\n got  %s\n want %s", b, want)
	}
	sup := "sg_1"
	long := strings.Repeat("é", 130)
	got = Signal(long, "T", protocol.ArtifactKindLink, "s", nil, "", &sup, nil, Content{URL: "https://x.test/a"})
	b, _ = json.Marshal(got)
	if strings.Contains(string(b), "request_for_approval") || !strings.Contains(string(b), `"title":"`+strings.Repeat("é", 120)+`"`) ||
		!strings.Contains(string(b), `"supersedes":"sg_1"`) || !strings.Contains(string(b), `"points":[]`) ||
		!strings.Contains(string(b), `"url":"https://x.test/a"`) {
		t.Fatalf("%s", b)
	}
	if got := Signal("n", "T", protocol.ArtifactKindText, "s", nil, "Given", nil, nil, Content{}); got["title"] != "Given" {
		t.Fatalf("title %v", got["title"])
	}
}

func TestLink(t *testing.T) {
	dir := real(t)
	f := write(t, filepath.Join(dir, "talk.mp4"), []byte("video"))
	c, kind, err := Link("https://drive.example.com/team/talk.mp4", "talk.mp4", f, "")
	if err != nil || kind != protocol.ArtifactKindVideo || c.URL != "https://drive.example.com/team/talk.mp4" ||
		c.MediaType != "video/mp4" || c.Size == nil || *c.Size != 5 || c.SHA256 != sum([]byte("video")) {
		t.Fatalf("%+v %s %v", c, kind, err)
	}
	c, _, err = Link("https://x.test/d", "deck.pptx", "", protocol.ArtifactKindLink)
	if err != nil || c.Size != nil || c.SHA256 != "" {
		t.Fatalf("no local copy: %+v %v", c, err)
	}
	if _, _, err := Link("https://x.test/d", "d", filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("a missing local copy is an error")
	}
	// a local copy over the limit is fine: it is only measured, never sent
	big := write(t, filepath.Join(dir, "big.mov"), bytes.Repeat([]byte{0}, MaxBytes+1))
	if c, _, err := Link("https://x.test/big", "big.mov", big, ""); err != nil || *c.Size != MaxBytes+1 {
		t.Fatalf("big: %+v %v", c, err)
	}
}

func content(name string, data []byte, stored *protocol.StoredRef, url *string) protocol.ArtifactContent {
	var c protocol.ArtifactContent
	body := protocol.ArtifactBody{Type: "artifact.v1", Name: name, Kind: protocol.ArtifactKindText, Stored: stored, URL: url}
	c.Signal.Signal.ID = "sg_2"
	c.Signal.Signal.Body = body
	c.Signal.ChainVersion = 2
	if data != nil {
		s := base64.StdEncoding.EncodeToString(data)
		c.ContentB64 = &s
	}
	return c
}

func TestPull(t *testing.T) {
	dir := real(t)
	t.Chdir(dir)
	data := []byte("# Spec\n")
	ref := &protocol.StoredRef{Key: "k", Sha256: sum(data), MediaType: "text/markdown", Size: int64(len(data))}

	got, err := Pull(content("spec.md", data, ref, nil), "")
	if err != nil || got.Path != "spec.md" || got.Line() != "spec.md\tsg_2 (v2)" {
		t.Fatalf("%+v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "spec.md")); !bytes.Equal(b, data) {
		t.Fatalf("wrote %q", b)
	}
	out := filepath.Join(dir, "sub", "copy.md")
	if got, err := Pull(content("spec.md", data, ref, nil), out); err != nil || got.Path != out {
		t.Fatalf("-o: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, data) {
		t.Fatal("-o not written")
	}

	// A teammate's name never chooses a folder: only its last part is used.
	if got, err := Pull(content("../../evil.md", data, ref, nil), ""); err != nil || got.Path != "evil.md" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "evil.md")); err == nil {
		t.Fatal("written outside the folder")
	}
	for _, name := range []string{"..", ".", "", "/"} {
		if _, err := Pull(content(name, data, ref, nil), ""); err == nil || !strings.Contains(err.Error(), "-o") {
			t.Errorf("name %q: %v", name, err)
		}
	}

	// A link: nothing written, the url given back.
	url := "https://x.test/talk.mp4"
	if got, err := Pull(content("talk.mp4", nil, nil, &url), ""); err != nil || got.Path != "" || got.Line() != url {
		t.Fatalf("link: %+v %v", got, err)
	}
	if got, _ := Pull(content("x", nil, nil, nil), ""); got.Line() != "no stored content" {
		t.Fatalf("nothing: %q", got.Line())
	}

	// Content that is not what the server stored is refused, and nothing is written.
	bad := sum([]byte("other"))
	_, err = Pull(content("tampered.md", data, &protocol.StoredRef{Sha256: bad}, nil), "")
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tampered.md")); err == nil {
		t.Fatal("tampered content written")
	}
	c := content("b.md", nil, nil, nil)
	notB64 := "%%%"
	c.ContentB64 = &notB64
	if _, err := Pull(c, ""); err == nil {
		t.Fatal("content that is not base64 is an error")
	}
	c = content("b.md", data, nil, nil)
	c.Signal.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "x"}
	if _, err := Pull(c, ""); err == nil || !strings.Contains(err.Error(), "not an artifact") {
		t.Fatalf("not an artifact: %v", err)
	}
}
