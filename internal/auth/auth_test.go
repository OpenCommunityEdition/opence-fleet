package auth

import (
	"context"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
)

func TestPasswords(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "wrong") || CheckPassword("nope", "x") {
		t.Fatal("argon2id")
	}
}

func TestFlow(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	a := New(s, 12, 240)
	ctx := context.Background()
	code, err := a.SetupCode(ctx)
	if err != nil || code == "" || !a.SetupOpen(ctx) {
		t.Fatal(err)
	}
	if _, _, err := a.Setup(ctx, "wrong", "paul", "a long password", "1.2.3.4"); err == nil {
		t.Fatal("a wrong code set up")
	}
	token, owner, err := a.Setup(ctx, code, "paul", "a long password", "1.2.3.4")
	if err != nil || owner.Role != Owner {
		t.Fatal(err)
	}
	if a.SetupOpen(ctx) {
		t.Fatal("the setup is still open")
	}
	if _, _, err := a.Setup(ctx, code, "eve", "a long password", "1.2.3.4"); err == nil {
		t.Fatal("the setup ran twice")
	}
	if p := a.Session(ctx, token); p == nil || p.Name != "paul" {
		t.Fatal("no session")
	}
	invite, err := a.MakeInvite(ctx, owner, Moderator, "a mod")
	if err != nil {
		t.Fatal(err)
	}
	_, mod, err := a.AcceptInvite(ctx, invite, "mod", "another long one", "1.2.3.5")
	if err != nil || mod.Role != Moderator {
		t.Fatal(err)
	}
	if _, _, err := a.AcceptInvite(ctx, invite, "mod2", "another long one", "1.2.3.5"); err == nil {
		t.Fatal("an invite was used twice")
	}
	if _, err := a.MakeInvite(ctx, mod, Admin, ""); err == nil {
		t.Fatal("a moderator invited an admin")
	}
	api, err := a.MakeToken(ctx, owner, "ci", Admin)
	if err != nil {
		t.Fatal(err)
	}
	if p := a.Token(ctx, api); p == nil || p.Role != Admin || p.Actor() != "token:ci" {
		t.Fatal("no token principal")
	}
	// five wrong passwords lock the address
	for i := 0; i < 5; i++ {
		a.Login(ctx, "paul", "wrong", "9.9.9.9", false)
	}
	if _, _, err := a.Login(ctx, "paul", "a long password", "9.9.9.9", false); err != ErrLocked {
		t.Fatalf("not locked: %v", err)
	}
	if _, _, err := a.Login(ctx, "paul", "a long password", "8.8.8.8", false); err != nil {
		t.Fatal(err)
	}
	// a page that only listens keeps its session (Touch), an idle one ends
	now := time.Now()
	a.Now = func() time.Time { return now.Add(200 * time.Minute) }
	if !a.Touch(ctx, owner) {
		t.Fatal("a used session was not alive")
	}
	a.Now = func() time.Time { return now.Add(400 * time.Minute) }
	if a.Session(ctx, token) == nil {
		t.Fatal("a touched session ended")
	}
	a.Now = func() time.Time { return now.Add(641 * time.Minute) }
	if a.Session(ctx, token) != nil || a.Touch(ctx, owner) {
		t.Fatal("an idle session lived")
	}
	a.Now = time.Now
	// with no idle limit, a session lasts its length
	unlimited := New(s, 12, 0)
	token, _, err = unlimited.Login(ctx, "paul", "a long password", "8.8.8.8", false)
	if err != nil {
		t.Fatal(err)
	}
	unlimited.Now = func() time.Time { return now.Add(11 * time.Hour) }
	if unlimited.Session(ctx, token) == nil {
		t.Fatal("a session without an idle limit ended early")
	}
	unlimited.Now = func() time.Time { return now.Add(13 * time.Hour) }
	if unlimited.Session(ctx, token) != nil {
		t.Fatal("a session outlived its length")
	}
	// the last owner stays one
	if err := a.SetRole(ctx, owner, owner.UserID, Admin); err == nil {
		t.Fatal("the last owner was demoted")
	}
}
