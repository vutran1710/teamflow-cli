package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/minisign"
)

// throwawaySecret is a minisign secret key file made for one test (never a release key).
func throwawaySecret(t *testing.T, kdf string) (string, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	b := []byte("Ed" + kdf + "B2")
	b = append(b, make([]byte, 48)...)
	b = append(b, id[:]...)
	b = append(b, priv...)
	b = append(b, make([]byte, 32)...)
	return "untrusted comment: minisign secret key\n" + base64.StdEncoding.EncodeToString(b) + "\n", minisign.EncodePublicKey(id, priv.Public().(ed25519.PublicKey))
}

func sums(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(in, []byte(strings.Repeat("a", 64)+"  teamflow_linux_amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return in, in + ".minisig"
}

// Happy path: signs checksums.txt so the committed key verifies it.
func TestSignsWithTheReleaseKey(t *testing.T) {
	sec, pub := throwawaySecret(t, "\x00\x00")
	in, out := sums(t)
	var log bytes.Buffer
	if err := run(in, out, sec, pub, &log); err != nil {
		t.Fatal(err)
	}
	msg, _ := os.ReadFile(in)
	sig, _ := os.ReadFile(out)
	pk, _ := minisign.ParsePublicKey(pub)
	if err := minisign.Verify(pk, msg, sig); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sig), "\tfile:checksums.txt\n") {
		t.Fatalf("trusted comment:\n%s", sig)
	}
	if strings.Contains(log.String(), strings.Split(sec, "\n")[1]) {
		t.Fatal("printed the key")
	}
}

// Fail closed, and never print the key: no secret; the placeholder public key (none committed); a key that is not
// the committed one; an encrypted key; a garbled key; a missing checksums.txt. No signature file is left.
func TestRefusesAndNeverPrintsTheKey(t *testing.T) {
	sec, pub := throwawaySecret(t, "\x00\x00")
	otherSec, _ := throwawaySecret(t, "\x00\x00")
	encSec, encPub := throwawaySecret(t, "Sc")
	cases := map[string]struct {
		sec, pub, want string
		noInput        bool
	}{
		"no secret":     {"", pub, "TEAMFLOW_SIGNING_KEY is empty", false},
		"blank secret":  {"  \n", pub, "TEAMFLOW_SIGNING_KEY is empty", false},
		"placeholder":   {sec, "", "no release key is committed", false},
		"another key":   {otherSec, pub, "not the committed release key", false},
		"encrypted key": {encSec, encPub, "minisign -G -W", false},
		"garbled key":   {"untrusted comment: x\n" + strings.Repeat("Q", 40) + "$\n", pub, "not base64", false},
		"no input":      {sec, pub, "checksums.txt", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in, out := sums(t)
			if c.noInput {
				_ = os.Remove(in)
			}
			var log bytes.Buffer
			err := run(in, out, c.sec, c.pub, &log)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("a signature file was written")
			}
			for _, line := range strings.Split(c.sec, "\n") {
				if len(line) > 20 && strings.Contains(err.Error()+log.String(), line) {
					t.Fatal("printed the key")
				}
			}
		})
	}
}
