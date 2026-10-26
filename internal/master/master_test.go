package master

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type party struct {
	Seed      string `json:"seed"`
	PublicKey string `json:"public_key"`
	KeyID     string `json:"key_id"`
}

type vectors struct {
	Publisher party  `json:"publisher"`
	Master    party  `json:"master"`
	Instance  string `json:"instance"`
	ServerID  string `json:"server_id"`
	MeRequest struct {
		MasterKeyID   string `json:"master_key_id"`
		TS            string `json:"ts"`
		Nonce         string `json:"nonce"`
		Canonical     string `json:"canonical"`
		Authorization string `json:"authorization"`
	} `json:"me_request"`
	ProofNonce     string `json:"proof_nonce"`
	ProofSignature string `json:"proof_signature_hex"`
	SmallOrder     string `json:"small_order_key_refused"`
}

func load(t *testing.T) vectors {
	data, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The vectors are the master server's, byte for byte (when its checkout is
// beside this one).
func TestVectorsAreUpstreams(t *testing.T) {
	upstream := filepath.Join("..", "..", "..", "opence-master-server", "testdata", "vectors.json")
	theirs, err := os.ReadFile(upstream)
	if err != nil {
		t.Skip("no opence-master-server checkout beside this one")
	}
	ours, _ := os.ReadFile("testdata/vectors.json")
	if !bytes.Equal(ours, theirs) {
		t.Fatal("testdata/vectors.json differs from opence-master-server's: copy it again")
	}
}

func TestVectors(t *testing.T) {
	v := load(t)
	key, err := ParseSeed(v.Publisher.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if got := PublicKeyText(key.Public().(ed25519.PublicKey)); got != v.Publisher.PublicKey {
		t.Fatalf("public key %s", got)
	}
	if got := KeyID(key.Public().(ed25519.PublicKey)); got != v.Publisher.KeyID {
		t.Fatalf("key id %s", got)
	}
	if got := ServerID(v.Publisher.KeyID, v.Instance); got != v.ServerID {
		t.Fatalf("server id %s", got)
	}
	m := Master{URL: "https://master.example.org", PublicKey: v.Master.PublicKey}
	if m.KeyID() != v.Master.KeyID || v.MeRequest.MasterKeyID != v.Master.KeyID {
		t.Fatalf("master key id %s", m.KeyID())
	}
	if got := Canonical(v.MeRequest.MasterKeyID, "GET", "/v1/me", v.MeRequest.TS, v.MeRequest.Nonce, nil); got != v.MeRequest.Canonical {
		t.Fatalf("canonical %q", got)
	}
	var ts int64
	json.Unmarshal([]byte(v.MeRequest.TS), &ts)
	if got := Authorization(key, v.MeRequest.MasterKeyID, "GET", "/v1/me", nil, time.Unix(ts, 0), v.MeRequest.Nonce); got != v.MeRequest.Authorization {
		t.Fatalf("authorization %q", got)
	}
	if v.ProofNonce != "" {
		if got := Proof(key, v.ProofNonce); got != v.ProofSignature {
			t.Fatalf("proof %s", got)
		}
	}
	if v.SmallOrder != "" {
		if _, err := ParsePublicKey(v.SmallOrder); err == nil {
			t.Fatal("a small-order key parsed")
		}
	}
}

func TestMasters(t *testing.T) {
	list, err := ParseMasters("# none\n\n")
	if err != nil || len(list) != 1 || list[0] != Official {
		t.Fatal(list, err)
	}
	list, err = ParseMasters("https://a.example.org/ " + Official.PublicKey + " # one\n")
	if err != nil || len(list) != 1 || list[0].URL != "https://a.example.org" {
		t.Fatal(list, err)
	}
	if _, err := ParseMasters("https://a.example.org ed25519:00\n"); err == nil {
		t.Fatal("a bad key parsed")
	}
}

// Me signs the request as the master checks it, and reads the answers.
func TestMe(t *testing.T) {
	v := load(t)
	key, _ := ParseSeed(v.Publisher.Seed)
	masterKey, _ := ParseSeed(v.Master.Seed)
	answer := map[string]any{}
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, err := ParseAuthorization(r.Header.Get("Authorization"))
		if err != nil || !auth.Verify(key.Public().(ed25519.PublicKey), KeyID(masterKey.Public().(ed25519.PublicKey)), r.Method, r.URL.Path, nil) {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": "bad_signature"}})
			return
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(answer)
	}))
	defer server.Close()
	m := Master{URL: server.URL, PublicKey: v.Master.PublicKey}
	c := &Client{HTTP: server.Client()}
	answer = map[string]any{"ok": true, "key_id": v.Publisher.KeyID, "status": "approved", "publisher": map[string]any{
		"id": 3, "name": "Pablo", "since": "3 days", "status": "approved", "quota": 4,
		"servers": []map[string]any{{"id": "abc", "instance": "btb-eu-1", "listed": true, "probe": "ok"}}}}
	me, err := c.Me(context.Background(), m, key)
	if err != nil || me.Live() != 1 || me.Publisher.Quota != 4 || me.Words() != "approved: 1 of 4 servers live" {
		t.Fatalf("%v %+v", err, me)
	}
	status = http.StatusForbidden
	answer = map[string]any{"ok": false, "error": map[string]string{"code": "unknown_key", "message": "no"}}
	me, err = c.Me(context.Background(), m, key)
	if err != nil || me.Error == nil || me.Words() != "this master does not know the key: apply with this key" {
		t.Fatalf("%v %+v", err, me)
	}
	status = http.StatusOK
	answer = map[string]any{"ok": true, "status": "pending", "publisher": nil, "application": map[string]any{"status": "pending"}}
	if me, err = c.Me(context.Background(), m, key); err != nil || me.Status != "pending" || me.Publisher != nil {
		t.Fatalf("%v %+v", err, me)
	}
}
