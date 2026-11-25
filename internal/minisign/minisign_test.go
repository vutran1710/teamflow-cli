package minisign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// testKey is a throwaway minisign-format keypair made for one test (never a release key).
func testKey(t *testing.T) (pubText string, secText string, priv ed25519.PrivateKey, keyID [8]byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(keyID[:]); err != nil {
		t.Fatal(err)
	}
	return EncodePublicKey(keyID, pub), encodeSecret(keyID, priv, [2]byte{0, 0}), priv, keyID
}

// encodeSecret lays out a minisign secret key file (the checksum field is left zero: this parser does not check it).
func encodeSecret(keyID [8]byte, priv ed25519.PrivateKey, kdf [2]byte) string {
	b := []byte("Ed")
	b = append(b, kdf[:]...)
	b = append(b, 'B', '2')
	b = append(b, make([]byte, 32+8+8)...) // salt, opslimit, memlimit
	b = append(b, keyID[:]...)
	b = append(b, priv...)
	b = append(b, make([]byte, 32)...) // checksum
	return "untrusted comment: minisign encrypted secret key\n" + base64.StdEncoding.EncodeToString(b) + "\n"
}

// Happy path: a signature made here verifies with the matching public key.
func TestSignVerifyRoundTrip(t *testing.T) {
	pubText, secText, _, _ := testKey(t)
	sk, err := ParseSecretKey([]byte(secText))
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("abc  teamflow_linux_amd64.tar.gz\n")
	sig := Sign(sk, msg, "timestamp:1\tfile:checksums.txt")
	if err := Verify(pk, msg, sig); err != nil {
		t.Fatalf("verify: %v\n%s", err, sig)
	}
	if !strings.HasPrefix(string(sig), "untrusted comment: ") || !strings.Contains(string(sig), "\ntrusted comment: timestamp:1\tfile:checksums.txt\n") {
		t.Fatalf("not minisign's layout:\n%s", sig)
	}
	if sk.Public().String() != pk.String() {
		t.Fatalf("secret key's public half %s != %s", sk.Public(), pk)
	}
	// The public key file form (two lines) parses too.
	if _, err := ParsePublicKey("untrusted comment: minisign public key ABC\n" + pubText + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Sad paths: a changed message, a changed trusted comment (the global signature), another key with the same id,
// another key id, a prehashed signature (not checkable with the standard library), and malformed files.
func TestVerifyRefuses(t *testing.T) {
	pubText, secText, _, keyID := testKey(t)
	sk, _ := ParseSecretKey([]byte(secText))
	pk, _ := ParsePublicKey(pubText)
	msg := []byte("abc  teamflow_linux_amd64.tar.gz\n")
	sig := string(Sign(sk, msg, "timestamp:1\tfile:checksums.txt"))
	lines := strings.Split(sig, "\n")

	if err := Verify(pk, []byte("abd  teamflow_linux_amd64.tar.gz\n"), []byte(sig)); err == nil || !strings.Contains(err.Error(), "signature does not match") {
		t.Fatalf("changed message: %v", err)
	}
	tc := strings.Replace(sig, "file:checksums.txt", "file:other.txt", 1)
	if err := Verify(pk, msg, []byte(tc)); err == nil || !strings.Contains(err.Error(), "trusted comment") {
		t.Fatalf("changed trusted comment: %v", err)
	}
	// Another key under the same id.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	same, _ := ParsePublicKey(EncodePublicKey(keyID, otherPub))
	if err := Verify(same, msg, []byte(sig)); err == nil || !strings.Contains(err.Error(), "signature does not match") {
		t.Fatalf("other key, same id: %v", err)
	}
	// Another key id.
	otherPubText, _, _, _ := testKey(t)
	other, _ := ParsePublicKey(otherPubText)
	if err := Verify(other, msg, []byte(sig)); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Fatalf("other key id: %v", err)
	}
	// A prehashed ("ED") signature.
	raw, _ := base64.StdEncoding.DecodeString(lines[1])
	raw[1] = 'D'
	pre := strings.Replace(sig, lines[1], base64.StdEncoding.EncodeToString(raw), 1)
	if err := Verify(pk, msg, []byte(pre)); err == nil || !strings.Contains(err.Error(), "prehashed") {
		t.Fatalf("prehashed: %v", err)
	}
	for name, bad := range map[string]string{
		"empty":             "",
		"one line":          lines[0],
		"bad base64":        strings.Replace(sig, lines[1], "!!!", 1),
		"short signature":   strings.Replace(sig, lines[1], base64.StdEncoding.EncodeToString(raw[:20]), 1),
		"no trusted prefix": strings.Replace(sig, "trusted comment: ", "comment: ", 1),
		"short global":      strings.Replace(sig, lines[3], base64.StdEncoding.EncodeToString([]byte("x")), 1),
		"no untrusted line": strings.Join(lines[1:], "\n"),
	} {
		if err := Verify(pk, msg, []byte(bad)); err == nil {
			t.Fatalf("%s: verified", name)
		}
	}
}

// Sad paths: malformed public keys (empty, bad base64, wrong length, wrong algorithm).
func TestParsePublicKeyRefuses(t *testing.T) {
	pubText, _, _, _ := testKey(t)
	raw, _ := base64.StdEncoding.DecodeString(pubText)
	wrongAlg := append([]byte("XX"), raw[2:]...)
	for name, bad := range map[string]string{
		"empty":      "",
		"blank":      "  \n",
		"bad base64": "RW$$",
		"short":      base64.StdEncoding.EncodeToString(raw[:30]),
		"wrong alg":  base64.StdEncoding.EncodeToString(wrongAlg),
	} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("%s: parsed", name)
		}
	}
}

// Sad paths: an encrypted secret key (refused with how to make one this tool takes), a wrong algorithm, a short
// or garbled file. No error ever shows the key's text.
func TestParseSecretKeyRefuses(t *testing.T) {
	_, secText, priv, keyID := testKey(t)
	enc := encodeSecret(keyID, priv, [2]byte{'S', 'c'})
	b64 := strings.Split(secText, "\n")[1]
	raw, _ := base64.StdEncoding.DecodeString(b64)
	wrongAlg := append([]byte("XX"), raw[2:]...)
	cases := map[string]string{
		"encrypted":  enc,
		"empty":      "",
		"bad base64": "untrusted comment: x\n" + b64[:40] + "$$$\n",
		"short":      "untrusted comment: x\n" + base64.StdEncoding.EncodeToString(raw[:100]) + "\n",
		"wrong alg":  "untrusted comment: x\n" + base64.StdEncoding.EncodeToString(wrongAlg) + "\n",
	}
	for name, bad := range cases {
		_, err := ParseSecretKey([]byte(bad))
		if err == nil {
			t.Fatalf("%s: parsed", name)
		}
		for _, line := range strings.Split(bad, "\n") {
			if len(line) > 20 && strings.Contains(err.Error(), line) {
				t.Fatalf("%s: the error shows the key: %v", name, err)
			}
		}
	}
	if _, err := ParseSecretKey([]byte(enc)); err == nil || !strings.Contains(err.Error(), "minisign -G -W") {
		t.Fatalf("encrypted: %v", err)
	}
	// The bare base64 line (no comment line) is taken too, as GitHub secrets are often pasted that way.
	if _, err := ParseSecretKey([]byte(b64)); err != nil {
		t.Fatalf("bare line: %v", err)
	}
}
