// Package workflows checks the GitHub Actions workflows: every action is pinned to a full commit SHA, every
// workflow starts from no permissions, and the release key only reaches the signing job, which cannot write.
package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var releaseEnv = regexp.MustCompile(`(?m)^    environment: release(\s+#.*)?$`)

var pinned = regexp.MustCompile(`^\s*(- )?uses: [\w.-]+/[\w./-]+@[0-9a-f]{40} # v\d+\.\d+\.\d+\s*$`)

func workflows(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = string(b)
	}
	return out
}

// jobs splits a workflow's jobs: section by job (two-space-indented keys).
func jobs(text string) map[string]string {
	out := map[string]string{}
	_, after, ok := strings.Cut(text, "\njobs:\n")
	if !ok {
		return out
	}
	name := ""
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			name = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}
		if name != "" {
			out[name] += line + "\n"
		}
	}
	return out
}

// Every `uses:` names a full 40-character commit SHA with its version in a comment (a tag can be moved; a SHA
// cannot). Sad paths: a tag, a branch, a short SHA, no version comment.
func TestEveryActionIsPinnedToACommit(t *testing.T) {
	for name, text := range workflows(t) {
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "uses:") && !pinned.MatchString(line) {
				t.Errorf("%s:%d: not pinned to a commit SHA with a # vX.Y.Z comment: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
	for _, bad := range []string{
		"      - uses: actions/checkout@v7",
		"      - uses: actions/checkout@main",
		"      - uses: actions/checkout@3d3c42e",
		"      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
	} {
		if pinned.MatchString(bad) {
			t.Fatalf("the check takes %q", bad)
		}
	}
}

// Every workflow starts with no permissions; each job asks for its own.
func TestWorkflowsStartWithNoPermissions(t *testing.T) {
	for name, text := range workflows(t) {
		if !strings.Contains(text, "\npermissions: {}\n") {
			t.Errorf("%s: no top-level `permissions: {}`", name)
		}
		for job, body := range jobs(text) {
			if !strings.Contains(body, "    permissions:") {
				t.Errorf("%s: job %s names no permissions", name, job)
			}
		}
	}
}

// The release: the signing secret reaches only the sign job, which cannot write to the repo and runs no
// third-party action; the publish job can write but never sees the secret; only publish writes.
func TestReleaseKeepsTheKeyAwayFromWrites(t *testing.T) {
	text := workflows(t)["release.yml"]
	js := jobs(text)
	for _, want := range []string{"build", "sign", "verify", "publish"} {
		if js[want] == "" {
			t.Fatalf("release.yml has no %s job (has %v)", want, keys(js))
		}
	}
	for job, body := range js {
		hasSecret := strings.Contains(body, "secrets.TEAMFLOW_SIGNING_KEY")
		writes := strings.Contains(body, "contents: write")
		if hasSecret != (job == "sign") {
			t.Errorf("job %s: secret=%v", job, hasSecret)
		}
		if writes != (job == "publish") {
			t.Errorf("job %s: contents: write=%v", job, writes)
		}
	}
	for _, line := range strings.Split(js["sign"], "\n") {
		if strings.Contains(line, "uses:") && !strings.Contains(line, "uses: actions/") {
			t.Errorf("the sign job runs a third-party action: %s", strings.TrimSpace(line))
		}
	}
	if strings.Count(text, "secrets.TEAMFLOW_SIGNING_KEY") == 0 {
		t.Fatal("the release is never signed")
	}
}

// The key is an environment secret of `release` (only v* tags deploy there): sign and publish
// declare that environment, build and verify do not, and no other workflow names the key at all. Signing and
// checking pass the tag, so a signature names the release it was made for.
func TestReleaseKeyLivesInTheReleaseEnvironment(t *testing.T) {
	all := workflows(t)
	js := jobs(all["release.yml"])
	for job, body := range js {
		inEnv := releaseEnv.MatchString(body)
		if inEnv != (job == "sign" || job == "publish") {
			t.Errorf("job %s: environment release=%v", job, inEnv)
		}
	}
	for name, text := range all {
		if name != "release.yml" && strings.Contains(text, "TEAMFLOW_SIGNING_KEY") {
			t.Errorf("%s names TEAMFLOW_SIGNING_KEY", name)
		}
	}
	for job, body := range js {
		if job != "sign" && strings.Contains(body, "TEAMFLOW_SIGNING_KEY") {
			t.Errorf("job %s names TEAMFLOW_SIGNING_KEY", job)
		}
	}
	if !strings.Contains(js["sign"], `-tag "$GITHUB_REF_NAME"`) || !strings.Contains(js["publish"], `-verify -tag "$GITHUB_REF_NAME"`) {
		t.Error("sign and publish do not pass the tag to signsums")
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
