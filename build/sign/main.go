// Command sign signs Quay releases: an Ed25519 signature over SHA256SUMS, which Quay's self-update
// checks against the public keys built into internal/selfupdate before trusting any download.
//
//	go run ./build/sign keygen [FILE]   new key pair: the private key to FILE (default below, 0600),
//	                                    the public key printed for internal/selfupdate.PublicKeys
//	go run ./build/sign sign FILE       FILE.sig: the private key from $QUAY_SIGNING_KEY (base64),
//	                                    else the key file ($QUAY_SIGNING_KEY_FILE or the default)
//	go run ./build/sign verify FILE     check FILE.sig against the built-in public keys
//
// The default key file is ~/.config/quay/release-signing.key. Keep a copy somewhere safe: without
// it no Quay can be updated by itself again until users install a release with a new key by hand.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caoxg/quay/internal/selfupdate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func defaultKeyFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "quay", "release-signing.key")
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sign keygen [FILE] | sign FILE | verify FILE")
	}
	switch args[0] {
	case "keygen":
		file := defaultKeyFile()
		if len(args) > 1 {
			file = args[1]
		}
		if _, err := os.Stat(file); err == nil {
			return fmt.Errorf("%s exists; refusing to replace a signing key", file)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("private key: %s (keep a copy somewhere safe)\npublic key:  %s\n", file, base64.StdEncoding.EncodeToString(pub))
		return nil
	case "sign":
		if len(args) != 2 {
			return errors.New("usage: sign sign FILE")
		}
		key, err := privateKey()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		sig := ed25519.Sign(key, data)
		if err := selfupdate.Verify(data, []byte(base64.StdEncoding.EncodeToString(sig))); err != nil {
			return fmt.Errorf("the key is not one of the public keys built into Quay: %w", err)
		}
		if err := os.WriteFile(args[1]+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote " + args[1] + ".sig")
		return nil
	case "verify":
		if len(args) != 2 {
			return errors.New("usage: sign verify FILE")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(args[1] + ".sig")
		if err != nil {
			return err
		}
		if err := selfupdate.Verify(data, sig); err != nil {
			return err
		}
		fmt.Println("good signature")
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func privateKey() (ed25519.PrivateKey, error) {
	s := strings.TrimSpace(os.Getenv("QUAY_SIGNING_KEY"))
	if s == "" {
		file := os.Getenv("QUAY_SIGNING_KEY_FILE")
		if file == "" {
			file = defaultKeyFile()
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("no signing key: set QUAY_SIGNING_KEY or create %s (sign keygen): %w", file, err)
		}
		s = strings.TrimSpace(string(b))
	}
	seed, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the signing key is not a base64 Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
