package adapters

import (
	"slices"
	"testing"
)

type fakeAdapter struct {
	name      string
	detected  bool
	installed bool
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Detect() bool { return f.detected }
func (f *fakeAdapter) Install(string) ([]string, error) {
	f.installed = true
	return []string{"wrote x"}, nil
}
func (f *fakeAdapter) Uninstall() ([]string, error) {
	f.installed = false
	return []string{"removed x"}, nil
}
func (f *fakeAdapter) Installed() bool { return f.installed }

func TestRegistryStartsEmpty(t *testing.T) {
	if len(All()) != 0 {
		t.Fatalf("adapters before Task 9: %v", All())
	}
	if _, ok := Get("claude-code"); ok {
		t.Fatal("claude-code is not registered yet")
	}
}

func TestRegisterGetAllInSupportedOrder(t *testing.T) {
	defer Reset()
	Register(&fakeAdapter{name: "copilot"})
	Register(&fakeAdapter{name: "claude-code"})
	if a, ok := Get("claude-code"); !ok || a.Name() != "claude-code" {
		t.Fatal("Get after Register")
	}
	var names []string
	for _, a := range All() {
		names = append(names, a.Name())
	}
	if !slices.Equal(names, []string{"claude-code", "copilot"}) {
		t.Fatalf("All in supported order: %v", names)
	}
}

func TestRegisterUnsupportedPanics(t *testing.T) {
	defer Reset()
	defer func() {
		if recover() == nil {
			t.Fatal("registering an agent that is not supported must panic")
		}
	}()
	Register(&fakeAdapter{name: "codex"})
}

func TestSupported(t *testing.T) {
	if !slices.Equal(Supported, []string{"claude-code", "cursor", "antigravity", "copilot"}) {
		t.Fatal(Supported)
	}
	if !IsSupported("cursor") || IsSupported("opencode") {
		t.Fatal("IsSupported")
	}
}
