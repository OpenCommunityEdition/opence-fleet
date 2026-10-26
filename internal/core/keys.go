package core

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/master"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

type keyEntry struct {
	row  db.PublisherKey
	seed string
}

// keyRow is a publisher key ("": the fleet's default).
func (f *Fleet) keyRow(ctx context.Context, keyID string) (db.PublisherKey, error) {
	if keyID == "" {
		return f.Store.GetDefaultKey(ctx)
	}
	return f.Store.GetKey(ctx, keyID)
}

// instanceKey is the key an instance publishes with: its own, its
// profile's, or the fleet's default (nil: none, so it is not listed).
func (f *Fleet) instanceKey(ctx context.Context, i db.Instance, p *Profile) (*keyEntry, error) {
	id := deref(i.KeyID)
	if id == "" && p != nil {
		id = deref(p.KeyID)
	}
	row, err := f.keyRow(ctx, id)
	if err != nil {
		return nil, nil
	}
	seed, err := f.Key.OpenString(row.SeedEnc, "key:"+row.KeyID)
	if err != nil {
		return nil, err
	}
	return &keyEntry{row: row, seed: seed}, nil
}

// Keys lists the publisher keys.
func (f *Fleet) Keys(ctx context.Context) ([]db.PublisherKey, error) { return f.Store.ListKeys(ctx) }

// MakeKey makes a publisher key (32 random bytes); the first is the
// default.
func (f *Fleet) MakeKey(ctx context.Context, actor, label string) (string, error) {
	private, err := master.NewSeed()
	if err != nil {
		return "", err
	}
	return f.addKey(ctx, actor, label, private, "key.make")
}

// ImportKey keeps a publisher.key's seed (an operator who applied with it).
func (f *Fleet) ImportKey(ctx context.Context, actor, label, text string) (string, error) {
	private, err := master.ParseSeed(text)
	if err != nil {
		return "", Refuse(http.StatusBadRequest, "schema", "%v", err)
	}
	return f.addKey(ctx, actor, label, private, "key.import")
}

func (f *Fleet) addKey(ctx context.Context, actor, label string, private ed25519.PrivateKey, action string) (string, error) {
	public := private.Public().(ed25519.PublicKey)
	if master.SmallOrder(public) {
		return "", Refuse(http.StatusBadRequest, "schema", "the key has a small order: make another")
	}
	id := master.KeyID(public)
	if _, err := f.Store.GetKey(ctx, id); err == nil {
		return "", Refuse(http.StatusConflict, "taken", "key %s is the fleet's already", id)
	}
	keys, _ := f.Store.ListKeys(ctx)
	label = strings.TrimSpace(label)
	if label == "" {
		label = "key " + id[:6]
	}
	if err := f.Store.CreateKey(ctx, db.CreateKeyParams{KeyID: id, PublicKey: master.PublicKeyText(public),
		SeedEnc: f.Key.SealString(master.SeedText(private), "key:"+id), Label: label, IsDefault: boolInt(len(keys) == 0),
		CreatedAt: f.Now().Unix()}); err != nil {
		return "", err
	}
	f.Audit(ctx, actor, action, id, map[string]string{"label": label})
	go f.RefreshMe(context.Background(), id)
	// (servers that named no key now publish with the default)
	if len(keys) == 0 {
		f.bumpAll(ctx)
	}
	return id, nil
}

// bumpAll gives every instance a new generation (their environment changed).
func (f *Fleet) bumpAll(ctx context.Context) {
	instances, _ := f.Store.ListInstances(ctx)
	for _, i := range instances {
		f.Store.BumpInstanceGeneration(ctx, i.Slug)
	}
	f.PushAll(ctx)
}

// SetDefaultKey makes a key the fleet's default.
func (f *Fleet) SetDefaultKey(ctx context.Context, actor, id string) error {
	if _, err := f.Store.GetKey(ctx, id); err != nil {
		return NotFound("key " + id)
	}
	err := f.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.ClearDefaultKey(ctx); err != nil {
			return err
		}
		return q.SetDefaultKey(ctx, id)
	})
	if err != nil {
		return err
	}
	f.Audit(ctx, actor, "key.default", id, nil)
	f.bumpAll(ctx)
	return nil
}

// ForgetKey removes a key no instance or profile names, nor the default
// while others follow it.
func (f *Fleet) ForgetKey(ctx context.Context, actor, id string) error {
	row, err := f.Store.GetKey(ctx, id)
	if err != nil {
		return NotFound("key " + id)
	}
	if n, _ := f.Store.CountKeyInstances(ctx, &id); n > 0 {
		return Refuse(http.StatusConflict, "in_use", "%d servers publish with %s: give them another key first", n, id)
	}
	if row.IsDefault != 0 {
		if n, _ := f.Store.CountInstances(ctx); n > 0 {
			if keys, _ := f.Store.ListKeys(ctx); len(keys) > 1 {
				return Refuse(http.StatusConflict, "in_use", "%s is the default key: make another the default first", id)
			}
		}
	}
	if err := f.Store.DeleteKey(ctx, id); err != nil {
		return err
	}
	f.Audit(ctx, actor, "key.forget", id, nil)
	f.bumpAll(ctx)
	return nil
}

// SignNonce is the proof of key for an application: the key's signature of
// the bot's nonce, in hexadecimal.
func (f *Fleet) SignNonce(ctx context.Context, actor, id, nonce string) (string, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" || len(nonce) > 256 {
		return "", Refuse(http.StatusBadRequest, "schema", "paste the nonce the master's bot gave (opence-apply-…)")
	}
	row, err := f.Store.GetKey(ctx, id)
	if err != nil {
		return "", NotFound("key " + id)
	}
	seed, err := f.Key.OpenString(row.SeedEnc, "key:"+id)
	if err != nil {
		return "", err
	}
	private, err := master.ParseSeed(seed)
	if err != nil {
		return "", err
	}
	f.Audit(ctx, actor, "key.sign", id, map[string]string{"nonce": nonce})
	return master.Proof(private, nonce), nil
}

// MeAnswers is a key's last answers from the masters, by URL.
func (f *Fleet) MeAnswers(key db.PublisherKey) map[string]*master.Me {
	out := map[string]*master.Me{}
	if key.MeCache != nil {
		json.Unmarshal([]byte(*key.MeCache), &out)
	}
	return out
}

// MeErrors is a refresh's failures (unreachable masters), by URL.
type meCache struct {
	Answers map[string]*master.Me `json:"answers"`
}

// RefreshMe asks every master what it has of a key (a signed GET /v1/me),
// keeps the answers, and raises the publisher's alerts.
func (f *Fleet) RefreshMe(ctx context.Context, id string) error {
	row, err := f.Store.GetKey(ctx, id)
	if err != nil {
		return NotFound("key " + id)
	}
	seed, err := f.Key.OpenString(row.SeedEnc, "key:"+id)
	if err != nil {
		return err
	}
	private, err := master.ParseSeed(seed)
	if err != nil {
		return err
	}
	client := &master.Client{HTTP: f.HTTP, Agent: "opence-fleet/" + Version}
	answers := map[string]*master.Me{}
	for _, m := range f.Masters {
		call, cancel := context.WithTimeout(ctx, 15*time.Second)
		me, err := client.Me(call, m, private)
		cancel()
		if err != nil {
			answers[m.URL] = &master.Me{Error: &master.Error{Code: "unreachable", Message: err.Error()}}
			continue
		}
		answers[m.URL] = me
		target := id + "@" + m.URL
		switch {
		case me.Status == "pending":
			f.raiseAlert(ctx, "publisher_pending", target, me.Words())
		case me.Status == "suspended" || me.Status == "revoked":
			f.raiseAlert(ctx, "publisher_suspended", target, me.Words())
		default:
			f.resolveAlert("publisher_pending", target)
			f.resolveAlert("publisher_suspended", target)
		}
		if me.Publisher != nil && me.Publisher.Quota > 0 && me.Live() >= me.Publisher.Quota {
			count, _ := f.Store.CountInstances(ctx)
			if int(count) > me.Publisher.Quota {
				f.raiseAlert(ctx, "quota_exceeded", target, me.Words())
			}
		} else {
			f.resolveAlert("quota_exceeded", target)
		}
	}
	data, _ := json.Marshal(answers)
	text := string(data)
	return f.Store.UpdateKeyMe(ctx, db.UpdateKeyMeParams{MeCache: &text, MeCachedAt: ptrInt(f.Now().Unix()), KeyID: id})
}

// mastersLoop asks the masters about every key every 5 minutes.
func (f *Fleet) mastersLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
		keys, _ := f.Store.ListKeys(ctx)
		for _, k := range keys {
			if err := f.RefreshMe(ctx, k.KeyID); err != nil {
				f.Log.Warn("the masters were not asked", "key", k.KeyID, "err", err)
			}
		}
	}
}

// ServerID is what a master calls an instance under a key.
func ServerID(keyID, slug string) string {
	if keyID == "" {
		return ""
	}
	return master.ServerID(keyID, slug)
}

// InstanceKeyID is the key id an instance publishes with ("" for none).
func InstanceKeyID(ctx context.Context, f *Fleet, i db.Instance, p *Profile) string {
	key, err := f.instanceKey(ctx, i, p)
	if err != nil || key == nil {
		return ""
	}
	return key.row.KeyID
}
