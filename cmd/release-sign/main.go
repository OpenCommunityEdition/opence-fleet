// release-sign signs a release's SHA256SUMS with the release key, which
// the fleet's updater checks before installing a release (package update).
//
//	release-sign keygen               a new release key: its seed (the
//	                                  RELEASE_SIGNING_KEY secret) and public key
//	release-sign sign <SHA256SUMS>    writes <SHA256SUMS>.sig, with the seed
//	                                  in RELEASE_SIGNING_KEY (64 hex digits)
//	release-sign verify <SHA256SUMS>  checks <SHA256SUMS>.sig against the
//	                                  fleet's release key (update.ReleaseKey)
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "keygen":
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return err
		}
		public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		fmt.Printf("seed (the RELEASE_SIGNING_KEY secret): %s\npublic key (internal/update/key.go): %s\n",
			hex.EncodeToString(seed), hex.EncodeToString(public))
		return nil
	case len(args) == 2 && args[0] == "sign":
		seed, err := hex.DecodeString(strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY")))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("RELEASE_SIGNING_KEY is the release key's seed: 64 hexadecimal digits")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		signature := ed25519.Sign(ed25519.NewKeyFromSeed(seed), data)
		return os.WriteFile(args[1]+".sig", []byte(hex.EncodeToString(signature)+"\n"), 0o644)
	case len(args) == 2 && args[0] == "verify":
		public, _ := hex.DecodeString(update.ReleaseKey)
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		text, err := os.ReadFile(args[1] + ".sig")
		if err != nil {
			return err
		}
		signature, err := hex.DecodeString(strings.TrimSpace(string(text)))
		if err != nil || !ed25519.Verify(public, data, signature) {
			return errors.New("the signature is not the release key's (update.ReleaseKey): is RELEASE_SIGNING_KEY " +
				"its seed?")
		}
		fmt.Println("the signature is the release key's")
		return nil
	}
	return errors.New("usage: release-sign keygen | release-sign sign <SHA256SUMS> | release-sign verify <SHA256SUMS>")
}
