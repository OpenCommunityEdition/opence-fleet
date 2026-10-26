// Package auth is the fleet's accounts: names and argon2id passwords (the
// parameters of RFC 9106's second recommendation), sessions kept in the
// database (32 random bytes in a cookie, stored hashed, revocable), the
// first start's setup link that makes the owner, invites for the team, API
// tokens for scripts, the roles (owner, admin, moderator, viewer), and the
// lockout of an address after five wrong passwords.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// The roles, lowest first.
const (
	Viewer    = "viewer"
	Moderator = "moderator"
	Admin     = "admin"
	Owner     = "owner"
)

// Roles is every role, highest first.
var Roles = []string{Owner, Admin, Moderator, Viewer}

var rank = map[string]int{Viewer: 1, Moderator: 2, Admin: 3, Owner: 4}

// AtLeast tells whether a role is need or above.
func AtLeast(role, need string) bool { return rank[role] >= rank[need] && rank[need] > 0 }

// ValidRole tells whether a role is one.
func ValidRole(role string) bool { return rank[role] > 0 }

// The argon2id parameters (RFC 9106, 4.: the second recommendation).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKey     = 32
)

// HashPassword hashes a password: $argon2id$v=19$m=…,t=…,p=…$salt$hash.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKey)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// CheckPassword tells whether a password is the hash's.
func CheckPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory uint32
	var iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckNewPassword refuses a password too short to keep.
func CheckNewPassword(password string) error {
	if len(password) < 10 {
		return errors.New("a password is 10 characters or more")
	}
	if len(password) > 256 {
		return errors.New("a password is 256 characters at most")
	}
	return nil
}

// CheckName refuses a user's name that is not one.
func CheckName(name string) error {
	if len(name) < 2 || len(name) > 32 {
		return errors.New("a name is 2 to 32 characters")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return errors.New("a name is letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

// Hash is a secret's SHA-256, as the database keeps it.
func Hash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Random is n random bytes in hexadecimal.
func Random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Principal is who a request is.
type Principal struct {
	UserID int64
	Name   string
	Role   string
	// the session's CSRF token ("" for an API token, which needs none)
	CSRF string
	// session or token
	Kind  string
	Label string
	// the session's token's hash (to revoke it)
	TokenHash []byte
}

// Actor is the principal for the audit log.
func (p *Principal) Actor() string {
	if p == nil {
		return "anonymous"
	}
	if p.Kind == "token" {
		return "token:" + p.Label
	}
	return "user:" + p.Name
}

// Can tells whether the principal's role is need or above.
func (p *Principal) Can(need string) bool { return p != nil && AtLeast(p.Role, need) }

// Auth is the accounts.
type Auth struct {
	Store *store.Store
	Now   func() time.Time
	// a session's length; "remember me" makes it 30 days
	Length time.Duration
	Idle   time.Duration

	mu       sync.Mutex
	failures map[string][]time.Time
	locked   map[string]time.Time
	checked  []time.Time
}

// New makes the accounts.
func New(s *store.Store, sessionHours int) *Auth {
	return &Auth{Store: s, Now: time.Now, Length: time.Duration(sessionHours) * time.Hour, Idle: 30 * time.Minute,
		failures: map[string][]time.Time{}, locked: map[string]time.Time{}}
}

// ErrLocked is an address locked out by its wrong passwords.
var ErrLocked = errors.New("five wrong passwords: this address is locked out for 10 minutes")

// ErrDenied is a wrong name or password.
var ErrDenied = errors.New("the name or the password is wrong")

// Locked tells whether an address is locked out.
func (a *Auth) Locked(address string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.locked[address]
	return ok && a.Now().Before(until)
}

func (a *Auth) fail(address string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	var recent []time.Time
	for _, t := range append(a.failures[address], now) {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	a.failures[address] = recent
	if len(recent) >= 5 {
		a.locked[address] = now.Add(10 * time.Minute)
		delete(a.failures, address)
	}
	if len(a.failures) > 65536 {
		a.failures = map[string][]time.Time{}
	}
}

// budget allows 30 password checks a minute in all (argon2id is costly).
func (a *Auth) budget() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	var recent []time.Time
	for _, t := range a.checked {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 30 {
		a.checked = recent
		return false
	}
	a.checked = append(recent, now)
	return true
}

// Login checks a name and password and makes a session: its token and
// CSRF token.
func (a *Auth) Login(ctx context.Context, name, password, address string, remember bool) (string, *Principal, error) {
	if a.Locked(address) {
		return "", nil, ErrLocked
	}
	if !a.budget() {
		return "", nil, errors.New("too many sign-ins this minute: try again in a minute")
	}
	user, err := a.Store.GetUserByName(ctx, strings.TrimSpace(name))
	if err != nil || user.Disabled != 0 || !CheckPassword(user.PasswordHash, password) {
		if err != nil {
			// (as long as a check, so that a name's existence does not show)
			CheckPassword("$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g", password)
		}
		a.fail(address)
		return "", nil, ErrDenied
	}
	return a.newSession(ctx, user, address, remember)
}

func (a *Auth) newSession(ctx context.Context, user db.User, address string, remember bool) (string, *Principal, error) {
	token := Random(32)
	csrf := Random(16)
	now := a.Now()
	length := a.Length
	if remember {
		length = 30 * 24 * time.Hour
	}
	if err := a.Store.CreateSession(ctx, db.CreateSessionParams{TokenHash: Hash(token), UserID: user.ID, Csrf: csrf,
		CreatedAt: now.Unix(), ExpiresAt: now.Add(length).Unix(), LastUsed: now.Unix(), Address: address,
		Remember: boolInt(remember)}); err != nil {
		return "", nil, err
	}
	return token, &Principal{UserID: user.ID, Name: user.Name, Role: user.Role, CSRF: csrf, Kind: "session",
		TokenHash: Hash(token)}, nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Session finds a session's principal (nil: none, expired or idle).
func (a *Auth) Session(ctx context.Context, token string) *Principal {
	if token == "" {
		return nil
	}
	s, err := a.Store.GetSession(ctx, Hash(token))
	if err != nil {
		return nil
	}
	now := a.Now()
	idle := s.Remember == 0 && now.Sub(time.Unix(s.LastUsed, 0)) > a.Idle
	if now.Unix() > s.ExpiresAt || idle || s.Disabled != 0 {
		a.Store.DeleteSession(ctx, s.TokenHash)
		return nil
	}
	if now.Unix()-s.LastUsed > 60 {
		a.Store.TouchSession(ctx, db.TouchSessionParams{LastUsed: now.Unix(), TokenHash: s.TokenHash})
	}
	return &Principal{UserID: s.UserID, Name: s.Name, Role: s.Role, CSRF: s.Csrf, Kind: "session", TokenHash: s.TokenHash}
}

// Logout ends a session.
func (a *Auth) Logout(ctx context.Context, token string) {
	a.Store.DeleteSession(ctx, Hash(token))
}

// Token finds an API token's principal (its role, at most its maker's).
func (a *Auth) Token(ctx context.Context, token string) *Principal {
	if !strings.HasPrefix(token, "ocf_") {
		return nil
	}
	t, err := a.Store.GetTokenByHash(ctx, Hash(token))
	if err != nil || t.UserDisabled != 0 {
		return nil
	}
	if t.LastUsed == nil || a.Now().Unix()-*t.LastUsed > 60 {
		now := a.Now().Unix()
		a.Store.TouchToken(ctx, db.TouchTokenParams{LastUsed: &now, ID: t.ID})
	}
	return &Principal{UserID: t.CreatedBy, Name: t.UserName, Role: t.Role, Kind: "token", Label: t.Label}
}

// MakeToken makes an API token for a user (a role at most theirs): the
// token, shown once.
func (a *Auth) MakeToken(ctx context.Context, by *Principal, label, role string) (string, error) {
	if !ValidRole(role) || !AtLeast(by.Role, role) {
		return "", fmt.Errorf("a token's role is at most yours (%s)", by.Role)
	}
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 64 {
		return "", errors.New("a token's label is 1 to 64 characters")
	}
	token := "ocf_" + Random(24)
	if _, err := a.Store.CreateToken(ctx, db.CreateTokenParams{TokenHash: Hash(token), Label: label, Role: role,
		CreatedBy: by.UserID, CreatedAt: a.Now().Unix()}); err != nil {
		return "", err
	}
	return token, nil
}

// SetupCode makes the first start's setup code (when there is no user):
// the code, which the setup link carries, once.
func (a *Auth) SetupCode(ctx context.Context) (string, error) {
	n, err := a.Store.CountUsers(ctx)
	if err != nil || n > 0 {
		return "", err
	}
	code := Random(24)
	if err := a.Store.SetSetting(ctx, db.SetSettingParams{Name: "setup_code_hash", Value: hex.EncodeToString(Hash(code))}); err != nil {
		return "", err
	}
	return code, nil
}

// SetupOpen tells whether the setup link is still to be used.
func (a *Auth) SetupOpen(ctx context.Context) bool {
	n, _ := a.Store.CountUsers(ctx)
	return n == 0 && a.Store.Setting(ctx, "setup_code_hash") != ""
}

// Setup makes the owner with the setup code (once).
func (a *Auth) Setup(ctx context.Context, code, name, password, address string) (string, *Principal, error) {
	want := a.Store.Setting(ctx, "setup_code_hash")
	got := hex.EncodeToString(Hash(code))
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		a.fail(address)
		return "", nil, errors.New("the setup code is not this fleet's (the first start printed the setup link)")
	}
	if n, _ := a.Store.CountUsers(ctx); n > 0 {
		return "", nil, errors.New("the fleet has its owner already: sign in")
	}
	if err := CheckName(name); err != nil {
		return "", nil, err
	}
	if err := CheckNewPassword(password); err != nil {
		return "", nil, err
	}
	id, err := a.Store.CreateUser(ctx, db.CreateUserParams{Name: name, PasswordHash: HashPassword(password), Role: Owner,
		CreatedAt: a.Now().Unix()})
	if err != nil {
		return "", nil, err
	}
	a.Store.DeleteSetting(ctx, "setup_code_hash")
	user, _ := a.Store.GetUser(ctx, id)
	return a.newSession(ctx, user, address, false)
}

// MakeInvite makes an invite (a role at most the inviter's, 7 days): its
// code, shown once.
func (a *Auth) MakeInvite(ctx context.Context, by *Principal, role, label string) (string, error) {
	if !ValidRole(role) || !AtLeast(by.Role, role) {
		return "", fmt.Errorf("an invite's role is at most yours (%s)", by.Role)
	}
	code := Random(18)
	err := a.Store.CreateInvite(ctx, db.CreateInviteParams{CodeHash: Hash(code), Role: role, Label: strings.TrimSpace(label),
		ExpiresAt: a.Now().Add(7 * 24 * time.Hour).Unix(), CreatedBy: by.UserID, CreatedAt: a.Now().Unix()})
	return code, err
}

// Invite reads an invite (nil: none, used or expired).
func (a *Auth) Invite(ctx context.Context, code string) *db.Invite {
	i, err := a.Store.GetInvite(ctx, Hash(code))
	if err != nil || i.UsedBy != nil || i.ExpiresAt < a.Now().Unix() {
		return nil
	}
	return &i
}

// AcceptInvite makes a user of an invite (once).
func (a *Auth) AcceptInvite(ctx context.Context, code, name, password, address string) (string, *Principal, error) {
	invite := a.Invite(ctx, code)
	if invite == nil {
		a.fail(address)
		return "", nil, errors.New("the invite was used, or is more than 7 days old: ask for another")
	}
	if err := CheckName(name); err != nil {
		return "", nil, err
	}
	if err := CheckNewPassword(password); err != nil {
		return "", nil, err
	}
	if _, err := a.Store.GetUserByName(ctx, name); err == nil {
		return "", nil, errors.New("the name is taken")
	}
	var id int64
	err := a.Store.Tx(ctx, func(q *db.Queries) error {
		var err error
		id, err = q.CreateUser(ctx, db.CreateUserParams{Name: name, PasswordHash: HashPassword(password), Role: invite.Role,
			CreatedAt: a.Now().Unix(), CreatedBy: &invite.CreatedBy})
		if err != nil {
			return err
		}
		n, err := q.UseInvite(ctx, db.UseInviteParams{UsedBy: &id, CodeHash: invite.CodeHash, ExpiresAt: a.Now().Unix()})
		if err == nil && n == 0 {
			err = errors.New("the invite was used just now")
		}
		return err
	})
	if err != nil {
		return "", nil, err
	}
	user, _ := a.Store.GetUser(ctx, id)
	return a.newSession(ctx, user, address, false)
}

// SetRole changes a user's role (the last owner stays one).
func (a *Auth) SetRole(ctx context.Context, by *Principal, userID int64, role string) error {
	if !ValidRole(role) || !AtLeast(by.Role, role) {
		return fmt.Errorf("a role at most yours (%s)", by.Role)
	}
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return errors.New("no such user")
	}
	if !AtLeast(by.Role, user.Role) {
		return errors.New("their role is above yours")
	}
	if user.Role == Owner && role != Owner {
		if n, _ := a.Store.CountOwners(ctx); n <= 1 {
			return errors.New("the fleet keeps an owner: make another owner first")
		}
	}
	if err := a.Store.UpdateUserRole(ctx, db.UpdateUserRoleParams{Role: role, ID: userID}); err != nil {
		return err
	}
	// (their sessions end: they sign in again with the new role)
	return a.Store.DeleteUserSessions(ctx, userID)
}

// SetDisabled disables (or enables) a user, ending their sessions.
func (a *Auth) SetDisabled(ctx context.Context, by *Principal, userID int64, disabled bool) error {
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return errors.New("no such user")
	}
	if user.ID == by.UserID {
		return errors.New("you cannot disable yourself")
	}
	if !AtLeast(by.Role, user.Role) {
		return errors.New("their role is above yours")
	}
	if disabled && user.Role == Owner {
		if n, _ := a.Store.CountOwners(ctx); n <= 1 {
			return errors.New("the fleet keeps an owner")
		}
	}
	if err := a.Store.SetUserDisabled(ctx, db.SetUserDisabledParams{Disabled: boolInt(disabled), ID: userID}); err != nil {
		return err
	}
	return a.Store.DeleteUserSessions(ctx, userID)
}

// ChangePassword changes one's own password (the old one checked), and
// ends one's other sessions.
func (a *Auth) ChangePassword(ctx context.Context, p *Principal, old, password string) error {
	user, err := a.Store.GetUser(ctx, p.UserID)
	if err != nil || !CheckPassword(user.PasswordHash, old) {
		return errors.New("the current password is wrong")
	}
	if err := CheckNewPassword(password); err != nil {
		return err
	}
	if err := a.Store.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{PasswordHash: HashPassword(password), ID: p.UserID}); err != nil {
		return err
	}
	sessions, _ := a.Store.ListSessions(ctx)
	for _, s := range sessions {
		if s.UserID == p.UserID && subtle.ConstantTimeCompare(s.TokenHash, p.TokenHash) != 1 {
			a.Store.DeleteSession(ctx, s.TokenHash)
		}
	}
	return nil
}

// ResetPassword sets a user's password from the command line (the owner
// who forgot theirs): their sessions end.
func (a *Auth) ResetPassword(ctx context.Context, name, password string) error {
	user, err := a.Store.GetUserByName(ctx, name)
	if err != nil {
		return errors.New("no such user")
	}
	if err := CheckNewPassword(password); err != nil {
		return err
	}
	if err := a.Store.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{PasswordHash: HashPassword(password), ID: user.ID}); err != nil {
		return err
	}
	return a.Store.DeleteUserSessions(ctx, user.ID)
}
