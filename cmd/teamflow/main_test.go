package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "version"} {
		var out, errOut bytes.Buffer
		if code := run([]string{arg}, &out, &errOut); code != 0 {
			t.Errorf("%s: exit %d", arg, code)
		}
		if out.String() != "teamflow dev\n" {
			t.Errorf("%s: printed %q", arg, out.String())
		}
	}
}

func TestVersionComesFromTheBuild(t *testing.T) {
	old := version
	version = "1.2.3"
	defer func() { version = old }()
	var out, errOut bytes.Buffer
	run([]string{"--version"}, &out, &errOut)
	if out.String() != "teamflow 1.2.3\n" {
		t.Errorf("printed %q", out.String())
	}
}

func TestAnUnknownCommandIsRefused(t *testing.T) {
	for _, args := range [][]string{{"frobnicate"}, {"--nope"}, {}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "usage: teamflow") {
			t.Errorf("%v: stdout %q, stderr %q", args, out.String(), errOut.String())
		}
	}
}
