package crypt

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	k := New()
	sealed := k.SealString("hunter22-password", "instance:a:web")
	if bytes.Contains(sealed, []byte("hunter22")) {
		t.Fatal("the secret is in the clear")
	}
	if plain, err := k.OpenString(sealed, "instance:a:web"); err != nil || plain != "hunter22-password" {
		t.Fatalf("%q %v", plain, err)
	}
	if _, err := k.OpenString(sealed, "instance:b:web"); err == nil {
		t.Fatal("opened under another aad")
	}
	if _, err := New().OpenString(sealed, "instance:a:web"); err == nil {
		t.Fatal("opened with another key")
	}
	if k.Seal(nil, "x") != nil {
		t.Fatal("an empty secret sealed to something")
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "fleet.key")
	k, made, err := Load(path)
	if err != nil || !made {
		t.Fatal(err, made)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	again, made, err := Load(path)
	if err != nil || made || again.raw != k.raw {
		t.Fatal("the key changed", err, made)
	}
	os.WriteFile(path, []byte("nope"), 0o600)
	if _, _, err := Load(path); err == nil {
		t.Fatal("a bad key loaded")
	}
}
