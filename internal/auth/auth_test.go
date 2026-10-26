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
	a := New(s, 12)
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
	// idle sessions end
	now := time.Now()
	a.Now = func() time.Time { return now.Add(31 * time.Minute) }
	if a.Session(ctx, token) != nil {
		t.Fatal("an idle session lived")
	}
	a.Now = time.Now
	// the last owner stays one
	if err := a.SetRole(ctx, owner, owner.UserID, Admin); err == nil {
		t.Fatal("the last owner was demoted")
	}
}
