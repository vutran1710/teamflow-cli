// Command signsums signs a release's checksums.txt with the release key, in minisign format (the legacy pure
// ed25519 "Ed" form, which `minisign -V` and `teamflow self-update` both check). The release workflow runs it; it
// uses the standard library only, so the runner needs no minisign.
//
//	TEAMFLOW_SIGNING_KEY=<minisign secret key> go run ./tools/signsums -tag v1.2.3 -in dist/checksums.txt
//	go run ./tools/signsums -verify -tag v1.2.3 -in dist/checksums.txt   # checks with the committed key; no secret
//
// The trusted comment (signed too) is "timestamp:<unix>\tfile:checksums.txt\ttag:<tag>": self-update and
// install.sh refuse a signature whose tag is not the release they are installing.
//
// The secret key comes only from the environment variable (an unencrypted key, `minisign -G -W`; the whole file or
// its base64 line). It fails closed: no key, no committed public key (the placeholder), a key that is not the
// committed one, or a signature that does not verify with the committed key all refuse, and no signature file is
// written. Nothing it prints contains the key.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/minisign"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
)

func main() {
	in := flag.String("in", filepath.Join("dist", "checksums.txt"), "the file to sign")
	out := flag.String("out", "", "the signature file (default: <in>.minisig)")
	check := flag.Bool("verify", false, "check the signature with the committed key instead of signing")
	tag := flag.String("tag", "", "the release tag (vX.Y.Z) the signature is for")
	flag.Parse()
	if *out == "" {
		*out = *in + ".minisig"
	}
	if *check {
		if err := verify(*in, *out, selfupdate.PublicKey, *tag); err != nil {
			fmt.Fprintln(os.Stderr, "signsums:", err)
			os.Exit(1)
		}
		fmt.Printf("%s: the signature is good (the committed release key)\n", filepath.Base(*in))
		return
	}
	secret := os.Getenv("TEAMFLOW_SIGNING_KEY")
	if err := run(*in, *out, secret, selfupdate.PublicKey, *tag, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "signsums:", err)
		os.Exit(1)
	}
}

var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func checkTag(tag string) error {
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("-tag must be the release tag vX.Y.Z (got %q)", tag)
	}
	return nil
}

func run(in, out, secret, committed, tag string, log io.Writer) error {
	if err := checkTag(tag); err != nil {
		return err
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("TEAMFLOW_SIGNING_KEY is empty; refusing to publish an unsigned release")
	}
	if strings.TrimSpace(committed) == "" {
		return errors.New("no release key is committed (internal/selfupdate/pubkey.go is the placeholder); commit the public key first")
	}
	pub, err := minisign.ParsePublicKey(committed)
	if err != nil {
		return fmt.Errorf("the committed public key: %v", err)
	}
	sk, err := minisign.ParseSecretKey([]byte(secret))
	if err != nil {
		return fmt.Errorf("TEAMFLOW_SIGNING_KEY: %v", err)
	}
	if sk.Public().String() != pub.String() {
		return errors.New("TEAMFLOW_SIGNING_KEY is not the committed release key's secret half")
	}
	msg, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("cannot read %s: %v", in, err)
	}
	sig := minisign.Sign(sk, msg, fmt.Sprintf("timestamp:%d\tfile:%s\ttag:%s", time.Now().Unix(), filepath.Base(in), tag))
	if err := minisign.Verify(pub, msg, sig); err != nil {
		return fmt.Errorf("the new signature does not verify with the committed key: %v", err)
	}
	if err := os.WriteFile(out, sig, 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %v", out, err)
	}
	id := pub.ID
	for i, j := 0, len(id)-1; i < j; i, j = i+1, j-1 {
		id[i], id[j] = id[j], id[i] // minisign shows the key id as a little-endian number
	}
	fmt.Fprintf(log, "signed %s with the release key %X -> %s\n", filepath.Base(in), id, filepath.Base(out))
	return nil
}

// verify checks sigFile over in with the committed key.
func verify(in, sigFile, committed, tag string) error {
	if err := checkTag(tag); err != nil {
		return err
	}
	if strings.TrimSpace(committed) == "" {
		return errors.New("no release key is committed (internal/selfupdate/pubkey.go is the placeholder)")
	}
	pub, err := minisign.ParsePublicKey(committed)
	if err != nil {
		return fmt.Errorf("the committed public key: %v", err)
	}
	msg, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("cannot read %s: %v", in, err)
	}
	sig, err := os.ReadFile(sigFile)
	if err != nil {
		return fmt.Errorf("cannot read %s: %v", sigFile, err)
	}
	comment, err := minisign.VerifyComment(pub, msg, sig)
	if err != nil {
		return err
	}
	return selfupdate.CheckSignedTag(comment, tag)
}
