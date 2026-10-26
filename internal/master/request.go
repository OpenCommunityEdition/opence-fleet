package master

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Scheme is the Authorization header's scheme.
const Scheme = "OpenCE-Ed25519"

// Label starts every signed request string.
const Label = "OpenCE-Master-v1"

// ClockWindow is how far a request's ts may be from the master's clock.
const ClockWindow = 300 * time.Second

// NonceWindow is how long a nonce is remembered.
const NonceWindow = 600 * time.Second

var (
	hexNonce   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	decimalTS  = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)
	errMissing = errors.New("no OpenCE-Ed25519 authorization")
)

// Auth is a parsed Authorization header.
type Auth struct {
	KeyID     string
	TS        string
	Time      int64
	Nonce     string
	Signature []byte
}

// BodyHash is the lowercase hexadecimal SHA-256 of a body (an empty body's
// is SHA-256 of no bytes).
func BodyHash(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// Canonical is the string a request's signature covers: the label, the
// receiving master's key id (16 lowercase hexadecimal digits: the key the
// sender pinned for that master), the method, the path, ts, the nonce and
// the body's hash, joined by line feeds. Naming the master stops a request
// signed for one master being replayed to another.
func Canonical(masterKeyID, method, path, ts, nonce string, body []byte) string {
	return Label + "\n" + masterKeyID + "\n" + method + "\n" + path + "\n" + ts + "\n" + nonce + "\n" +
		BodyHash(body)
}

// ParseAuthorization reads "OpenCE-Ed25519 keyid=…, ts=…, nonce=…, sig=…":
// the four parameters each once, in any order, separated by commas with
// optional spaces.
func ParseAuthorization(header string) (Auth, error) {
	var auth Auth
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, Scheme+" ") {
		return auth, errMissing
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(header[len(Scheme)+1:], ",") {
		part = strings.TrimSpace(part)
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return auth, errors.New("a parameter without a value")
		}
		if seen[name] {
			return auth, errors.New("a parameter given twice: " + name)
		}
		seen[name] = true
		switch name {
		case "keyid":
			if !ValidKeyID(value) {
				return auth, errors.New("keyid is 16 lowercase hexadecimal digits")
			}
			auth.KeyID = value
		case "ts":
			if !decimalTS.MatchString(value) {
				return auth, errors.New("ts is a decimal number of seconds")
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return auth, errors.New("ts is out of range")
			}
			auth.TS, auth.Time = value, n
		case "nonce":
			if !hexNonce.MatchString(value) {
				return auth, errors.New("nonce is 32 lowercase hexadecimal digits")
			}
			auth.Nonce = value
		case "sig":
			raw, err := base64.StdEncoding.DecodeString(value)
			if err != nil || len(raw) != ed25519.SignatureSize {
				return auth, errors.New("sig is the base64 of 64 bytes")
			}
			auth.Signature = raw
		default:
			return auth, errors.New("an unknown parameter: " + name)
		}
	}
	if auth.KeyID == "" || auth.TS == "" || auth.Nonce == "" || auth.Signature == nil {
		return auth, errors.New("keyid, ts, nonce and sig are all needed")
	}
	return auth, nil
}

// Verify checks a request's signature with a public key, for the master
// whose key id is masterKeyID (the receiving one).
func (a Auth) Verify(public ed25519.PublicKey, masterKeyID, method, path string, body []byte) bool {
	return ed25519.Verify(public, []byte(Canonical(masterKeyID, method, path, a.TS, a.Nonce, body)), a.Signature)
}

// InWindow tells whether the request's ts is within ClockWindow of now.
func (a Auth) InWindow(now time.Time) bool {
	difference := now.Unix() - a.Time
	if difference < 0 {
		difference = -difference
	}
	return difference <= int64(ClockWindow/time.Second)
}

// NewNonce is 16 random bytes in hexadecimal.
func NewNonce() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Authorization signs a request to the master whose key id is masterKeyID:
// the Authorization header's value.
func Authorization(private ed25519.PrivateKey, masterKeyID, method, path string, body []byte, now time.Time,
	nonce string) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	signature := ed25519.Sign(private, []byte(Canonical(masterKeyID, method, path, ts, nonce, body)))
	public := private.Public().(ed25519.PublicKey)
	return Scheme + " keyid=" + KeyID(public) + ", ts=" + ts + ", nonce=" + nonce + ", sig=" +
		base64.StdEncoding.EncodeToString(signature)
}

// SignBody is a list's Signature header: "ed25519=" and the base64 of the
// signature of the body's exact bytes.
func SignBody(private ed25519.PrivateKey, body []byte) string {
	return "ed25519=" + base64.StdEncoding.EncodeToString(ed25519.Sign(private, body))
}

// VerifyBody checks a Signature header against a body.
func VerifyBody(public ed25519.PublicKey, header string, body []byte) bool {
	value, ok := strings.CutPrefix(strings.TrimSpace(header), "ed25519=")
	if !ok {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(public, body, raw)
}
