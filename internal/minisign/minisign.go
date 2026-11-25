// Package minisign reads and writes minisign-format (https://jedisct1.github.io/minisign/) ed25519 keys and
// signatures with the standard library only.
//
// Only the legacy, pure ed25519 signature ("Ed") is made and checked: the prehashed form ("ED", minisign's default
// since 0.10) hashes with BLAKE2b, which the standard library does not have. `minisign -V` checks both forms, so a
// file signed here verifies with the stock minisign tool.
package minisign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	untrustedPrefix = "untrusted comment: "
	trustedPrefix   = "trusted comment: "
	keyIDLen        = 8
	pubLen          = 2 + keyIDLen + ed25519.PublicKeySize                            // "Ed" keyid pk
	sigLen          = 2 + keyIDLen + ed25519.SignatureSize                            // "Ed" keyid sig
	secLen          = 2 + 2 + 2 + 32 + 8 + 8 + keyIDLen + ed25519.PrivateKeySize + 32 // alg kdf chk salt ops mem | keyid sk checksum
)

var algEd = [2]byte{'E', 'd'}

// PublicKey is a minisign public key: its 8-byte id and the ed25519 key.
type PublicKey struct {
	ID  [keyIDLen]byte
	Key ed25519.PublicKey
}

// String is the key's one-line base64 form (what `minisign -P` takes).
func (p PublicKey) String() string { return EncodePublicKey(p.ID, p.Key) }

// EncodePublicKey is the one-line base64 form of a public key.
func EncodePublicKey(id [keyIDLen]byte, key ed25519.PublicKey) string {
	b := append(algEd[:], id[:]...)
	return base64.StdEncoding.EncodeToString(append(b, key...))
}

// ParsePublicKey takes the one-line base64 key, or a whole minisign.pub file (comment line, then the key).
func ParsePublicKey(text string) (PublicKey, error) {
	line := lastLine(text)
	if line == "" {
		return PublicKey{}, errors.New("minisign: the public key is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return PublicKey{}, errors.New("minisign: the public key is not base64")
	}
	if len(raw) != pubLen {
		return PublicKey{}, fmt.Errorf("minisign: the public key is %d bytes, not %d", len(raw), pubLen)
	}
	if [2]byte(raw[:2]) != algEd {
		return PublicKey{}, errors.New("minisign: the public key is not an ed25519 (Ed) key")
	}
	var p PublicKey
	copy(p.ID[:], raw[2:2+keyIDLen])
	p.Key = ed25519.PublicKey(bytes.Clone(raw[2+keyIDLen:]))
	return p, nil
}

// SecretKey is an unencrypted minisign secret key.
type SecretKey struct {
	ID  [keyIDLen]byte
	key ed25519.PrivateKey
}

// Public is the secret key's public half.
func (s SecretKey) Public() PublicKey {
	return PublicKey{ID: s.ID, Key: s.key.Public().(ed25519.PublicKey)}
}

// ParseSecretKey takes an unencrypted minisign secret key file (`minisign -G -W`), or its base64 line alone. Its
// errors never quote the key.
func ParseSecretKey(file []byte) (SecretKey, error) {
	line := lastLine(string(file))
	if line == "" {
		return SecretKey{}, errors.New("minisign: the secret key is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return SecretKey{}, errors.New("minisign: the secret key is not base64")
	}
	if len(raw) != secLen {
		return SecretKey{}, fmt.Errorf("minisign: the secret key is %d bytes, not %d", len(raw), secLen)
	}
	if [2]byte(raw[:2]) != algEd {
		return SecretKey{}, errors.New("minisign: the secret key is not an ed25519 (Ed) key")
	}
	if raw[2] != 0 || raw[3] != 0 {
		return SecretKey{}, errors.New("minisign: the secret key is encrypted; make an unencrypted one with `minisign -G -W` (it lives only in the repo secret)")
	}
	at := 2 + 2 + 2 + 32 + 8 + 8
	var s SecretKey
	copy(s.ID[:], raw[at:at+keyIDLen])
	at += keyIDLen
	seedAndPub := raw[at : at+ed25519.PrivateKeySize]
	s.key = ed25519.NewKeyFromSeed(seedAndPub[:ed25519.SeedSize])
	if !bytes.Equal(s.key[ed25519.SeedSize:], seedAndPub[ed25519.SeedSize:]) {
		return SecretKey{}, errors.New("minisign: the secret key is damaged (its public half does not match its seed)")
	}
	return s, nil
}

// Sign makes a minisign signature file (legacy "Ed" form) of msg. trustedComment is signed too (one line).
func Sign(s SecretKey, msg []byte, trustedComment string) []byte {
	sig := ed25519.Sign(s.key, msg)
	head := append(algEd[:], s.ID[:]...)
	head = append(head, sig...)
	global := ed25519.Sign(s.key, append(bytes.Clone(sig), trustedComment...))
	var b strings.Builder
	b.WriteString(untrustedPrefix + "signature from the teamflow release key\n")
	b.WriteString(base64.StdEncoding.EncodeToString(head) + "\n")
	b.WriteString(trustedPrefix + trustedComment + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(global) + "\n")
	return []byte(b.String())
}

// Verify checks a minisign signature file over msg: the signature itself and the signed trusted comment.
func Verify(p PublicKey, msg, sigFile []byte) error {
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 {
		return errors.New("minisign: the signature file is not four lines")
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) && !strings.HasPrefix(lines[0], "untrusted comment:") {
		return errors.New("minisign: the signature file does not start with an untrusted comment")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil {
		return errors.New("minisign: the signature is not base64")
	}
	if len(raw) != sigLen {
		return fmt.Errorf("minisign: the signature is %d bytes, not %d", len(raw), sigLen)
	}
	switch [2]byte(raw[:2]) {
	case algEd:
	case [2]byte{'E', 'D'}:
		return errors.New("minisign: a prehashed (ED) signature cannot be checked here; sign with the legacy form")
	default:
		return errors.New("minisign: the signature is not an ed25519 signature")
	}
	if [keyIDLen]byte(raw[2:2+keyIDLen]) != p.ID {
		return errors.New("minisign: the file was signed by another key")
	}
	sig := raw[2+keyIDLen:]
	if !ed25519.Verify(p.Key, msg, sig) {
		return errors.New("minisign: the signature does not match the file")
	}
	if !strings.HasPrefix(lines[2], trustedPrefix) {
		return errors.New("minisign: the signature file has no trusted comment")
	}
	comment := strings.TrimPrefix(lines[2], trustedPrefix)
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(global) != ed25519.SignatureSize {
		return errors.New("minisign: the trusted comment's signature is malformed")
	}
	if !ed25519.Verify(p.Key, append(bytes.Clone(sig), comment...), global) {
		return errors.New("minisign: the trusted comment's signature does not match")
	}
	return nil
}

// lastLine is the last non-blank line: the key itself in a key file.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
