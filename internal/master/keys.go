// Package master is the master servers' protocol from a publisher's side
// (docs/master_protocol.md in the game's repository): publisher keys, key
// ids and server ids, the OpenCE-Ed25519 request signature, the proof of
// key an application asks for, masters.txt, and the signed GET /v1/me that
// the Masters page shows. keys.go and request.go are the master server's
// internal/sign, re-homed; testdata/vectors.json is a byte-identical copy of
// its vectors, which the tests check the code against.
package master

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"
)

// PublicKeyPrefix starts a public key written as text.
const PublicKeyPrefix = "ed25519:"

var (
	hexKey   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexKeyID = regexp.MustCompile(`^[0-9a-f]{16}$`)
	instance = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ParseSeed reads a key file's text: 64 hexadecimal digits (the Ed25519
// seed) on the first line that is not empty, anything after '#' a comment.
func ParseSeed(text string) (ed25519.PrivateKey, error) {
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if !hexKey.MatchString(line) {
			return nil, errors.New("a key file holds 64 hexadecimal digits")
		}
		seed, _ := hex.DecodeString(line)
		return ed25519.NewKeyFromSeed(seed), nil
	}
	return nil, errors.New("the key file is empty")
}

// LoadSeed reads a key: the 64 hexadecimal digits themselves, or a file
// holding them.
func LoadSeed(value string) (ed25519.PrivateKey, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if hexKey.MatchString(trimmed) {
		return ParseSeed(trimmed)
	}
	data, err := os.ReadFile(value)
	if err != nil {
		return nil, err
	}
	return ParseSeed(string(data))
}

// NewSeed makes a key pair from random bytes.
func NewSeed() (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	return private, err
}

// SeedText is a key file's line for a key.
func SeedText(private ed25519.PrivateKey) string {
	return hex.EncodeToString(private.Seed())
}

// PublicKeyText writes a public key as "ed25519:" and 64 lowercase
// hexadecimal digits.
func PublicKeyText(public ed25519.PublicKey) string {
	return PublicKeyPrefix + hex.EncodeToString(public)
}

// ParsePublicKey reads "ed25519:<64 hex>" (the prefix may be left out),
// refusing a key of small order, whose signatures anyone can make.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.TrimPrefix(text, PublicKeyPrefix)
	if !hexKey.MatchString(text) {
		return nil, errors.New("a public key is ed25519: and 64 hexadecimal digits")
	}
	raw, _ := hex.DecodeString(text)
	if SmallOrder(raw) {
		return nil, errors.New("the public key has a small order")
	}
	return ed25519.PublicKey(raw), nil
}

// KeyIDBytes is the first 8 bytes of SHA-256 of a public key's 32 bytes.
func KeyIDBytes(public ed25519.PublicKey) [8]byte {
	digest := sha256.Sum256(public)
	var id [8]byte
	copy(id[:], digest[:8])
	return id
}

// KeyID is KeyIDBytes in lowercase hexadecimal: 16 characters.
func KeyID(public ed25519.PublicKey) string {
	id := KeyIDBytes(public)
	return hex.EncodeToString(id[:])
}

// ValidKeyID tells whether text is a key id's form.
func ValidKeyID(text string) bool {
	return hexKeyID.MatchString(text)
}

// ValidInstance tells whether text is an instance name: 1 to 32 of
// a-z, 0-9, '-' and '_', starting with a letter or digit.
func ValidInstance(text string) bool {
	return instance.MatchString(text)
}

// ServerID is the first 16 lowercase hexadecimal digits of
// SHA-256(key id as its 16 characters of text, then the instance's text).
func ServerID(keyID, instanceName string) string {
	digest := sha256.Sum256([]byte(keyID + instanceName))
	return hex.EncodeToString(digest[:8])
}

var fieldPrime, _ = new(big.Int).SetString("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffed", 16)

// SmallOrder tells whether an Ed25519 public key's point has a small order,
// as the game checks it (p2p_crypto.c): its X25519 form, u = (1+y)/(1-y),
// multiplied by a clamped scalar (a multiple of 8) is zero.
func SmallOrder(public []byte) bool {
	if len(public) != 32 {
		return true
	}
	little := make([]byte, 32)
	copy(little, public)
	little[31] &= 0x7f
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		little[i], little[j] = little[j], little[i]
	}
	y := new(big.Int).SetBytes(little)
	y.Mod(y, fieldPrime)
	one := big.NewInt(1)
	numerator := new(big.Int).Add(one, y)
	denominator := new(big.Int).Sub(one, y)
	denominator.Mod(denominator, fieldPrime)
	u := new(big.Int)
	if denominator.Sign() != 0 {
		inverse := new(big.Int).ModInverse(denominator, fieldPrime)
		u.Mul(numerator, inverse)
		u.Mod(u, fieldPrime)
	}
	encoded := u.FillBytes(make([]byte, 32))
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		encoded[i], encoded[j] = encoded[j], encoded[i]
	}
	scalar := make([]byte, 32)
	scalar[0] = 1
	private, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return true
	}
	peer, err := ecdh.X25519().NewPublicKey(encoded)
	if err != nil {
		return true
	}
	_, err = private.ECDH(peer)
	return err != nil
}

// Describe is a key's public text and id, for printing.
func Describe(public ed25519.PublicKey) string {
	return fmt.Sprintf("public key %s\nkey id     %s", PublicKeyText(public), KeyID(public))
}
