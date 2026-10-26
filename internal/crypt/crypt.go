// Package crypt keeps the fleet's secrets at rest: the instances' web
// passwords, the publisher keys' seeds, the webhooks' URLs and the join
// passwords are sealed with fleet.key (XChaCha20-Poly1305), so that a copy
// of the database without the key reveals none of them. The key is made at
// the first start and backed up with the database.
package crypt

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// Key seals and opens secrets.
type Key struct {
	raw [chacha20poly1305.KeySize]byte
}

// Load reads the key file (64 hexadecimal digits), making it (mode 0600)
// when there is none.
func Load(path string) (*Key, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		k := &Key{}
		if _, err := rand.Read(k.raw[:]); err != nil {
			return nil, false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, false, err
		}
		// (O_EXCL: two starts at once do not make two keys)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, false, err
		}
		_, err = f.WriteString(hex.EncodeToString(k.raw[:]) + "\n")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return k, true, err
	}
	if err != nil {
		return nil, false, err
	}
	k, err := Parse(string(data))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w (restore the key from its backup)", path, err)
	}
	return k, false, nil
}

// Parse reads a key's text.
func Parse(text string) (*Key, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != chacha20poly1305.KeySize {
		return nil, errors.New("the secret key is 64 hexadecimal digits")
	}
	k := &Key{}
	copy(k.raw[:], raw)
	return k, nil
}

// New makes a key of random bytes (for tests).
func New() *Key {
	k := &Key{}
	_, _ = rand.Read(k.raw[:])
	return k
}

// Seal encrypts a secret, bound to what it is (aad, such as
// "instance:btb-eu-1:web_password"), so that a sealed value moved to another
// row does not open there. An empty secret seals to nothing.
func (k *Key) Seal(plain []byte, aad string) []byte {
	if len(plain) == 0 {
		return nil
	}
	aead, _ := chacha20poly1305.NewX(k.raw[:])
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte(aad))
}

// Open decrypts what Seal made with the same aad.
func (k *Key) Open(sealed []byte, aad string) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	aead, _ := chacha20poly1305.NewX(k.raw[:])
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("a sealed secret is too short")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(aad))
	if err != nil {
		return nil, errors.New("a secret does not open with fleet.key (another fleet's key?)")
	}
	return plain, nil
}

// SealString and OpenString are Seal and Open of text.
func (k *Key) SealString(plain, aad string) []byte { return k.Seal([]byte(plain), aad) }

// OpenString opens a sealed text.
func (k *Key) OpenString(sealed []byte, aad string) (string, error) {
	plain, err := k.Open(sealed, aad)
	return string(plain), err
}
