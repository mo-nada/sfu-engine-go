// Package token creates and verifies SFU join tokens.
//
// A join token is a JWT signed with HMAC-SHA256 (HS256). A tenant's backend
// signs one per user with the tenant's API key (as the issuer) and secret.
// The browser sends it in the signaling "join" message. Any language with a
// JWT library can create tokens; this package is for Go backends and the SFU.
//
// Claims:
//
//	iss     tenant API key
//	sub     participant identity (1–64 letters, digits, '-' or '_')
//	room    room name (same rules)
//	iat, nbf, exp   Unix seconds; exp is required and at most MaxTTL ahead
//	grants  {publish, subscribe, sources}; sources empty means all sources
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Leeway tolerates small clock differences between the tenant and the SFU.
const Leeway = 30 * time.Second

// MaxTTL is the longest accepted remaining lifetime of a token. Tokens cannot
// be revoked, so they must be short-lived.
const MaxTTL = 24 * time.Hour

// Grants are what the participant may do in the room.
type Grants struct {
	Publish   bool `json:"publish"`
	Subscribe bool `json:"subscribe"`
	// Sources limits what may be published: "camera", "microphone",
	// "screen", "screen-audio". Empty means all sources.
	Sources []string `json:"sources,omitempty"`
}

// AllowsSource reports whether source may be published.
func (g Grants) AllowsSource(source string) bool {
	return g.Publish && (len(g.Sources) == 0 || slices.Contains(g.Sources, source))
}

// Claims is the token payload.
type Claims struct {
	Issuer    string `json:"iss"` // tenant API key
	Subject   string `json:"sub"` // participant identity
	Room      string `json:"room"`
	IssuedAt  int64  `json:"iat,omitempty"`
	NotBefore int64  `json:"nbf,omitempty"`
	ExpiresAt int64  `json:"exp"`
	Grants    Grants `json:"grants"`
}

// Errors returned by Verify. Callers should not reveal which one occurred to
// the client; log it instead.
var (
	ErrMalformed     = errors.New("token: malformed")
	ErrAlgorithm     = errors.New("token: unsupported algorithm")
	ErrUnknownIssuer = errors.New("token: unknown API key")
	ErrSignature     = errors.New("token: bad signature")
	ErrExpired       = errors.New("token: expired")
	ErrNotYetValid   = errors.New("token: not valid yet")
	ErrInvalidClaims = errors.New("token: invalid claims")
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Strict decoding rejects non-canonical encodings, so each token has exactly
// one valid string form.
var b64 = base64.RawURLEncoding.Strict()

const header = `{"alg":"HS256","typ":"JWT"}`

// New returns claims for identity in room, valid for ttl from now, with
// publish and subscribe granted for all sources.
func New(apiKey, identity, room string, ttl time.Duration) Claims {
	now := time.Now()
	return Claims{
		Issuer:    apiKey,
		Subject:   identity,
		Room:      room,
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		Grants:    Grants{Publish: true, Subscribe: true},
	}
}

// Sign returns the signed token for c.
func Sign(secret string, c Claims) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString([]byte(header)) + "." + b64.EncodeToString(payload)
	return signing + "." + b64.EncodeToString(mac(secret, signing)), nil
}

// Verify checks the token's algorithm, signature and validity window at now.
// secretFor returns the secret for an API key, or false if the key is unknown.
func Verify(tok string, secretFor func(apiKey string) (string, bool), now time.Time) (Claims, error) {
	parts := strings.Split(tok, ".")
	// The decoder skips '\r' and '\n'; reject them so encodings stay unique.
	if len(parts) != 3 || strings.ContainsAny(tok, "\r\n") {
		return Claims{}, ErrMalformed
	}
	rawHeader, err := b64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return Claims{}, ErrMalformed
	}
	// Only HS256: rejects "none" and algorithm-confusion attacks.
	if h.Alg != "HS256" {
		return Claims{}, ErrAlgorithm
	}
	rawPayload, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(rawPayload, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	secret, ok := secretFor(c.Issuer)
	if !ok {
		return Claims{}, ErrUnknownIssuer
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !hmac.Equal(sig, mac(secret, parts[0]+"."+parts[1])) {
		return Claims{}, ErrSignature
	}
	if c.ExpiresAt == 0 || now.After(time.Unix(c.ExpiresAt, 0).Add(Leeway)) {
		return Claims{}, ErrExpired
	}
	if time.Unix(c.ExpiresAt, 0).After(now.Add(MaxTTL + Leeway)) {
		return Claims{}, fmt.Errorf("%w: expires more than %s from now", ErrInvalidClaims, MaxTTL)
	}
	if c.NotBefore != 0 && now.Add(Leeway).Before(time.Unix(c.NotBefore, 0)) {
		return Claims{}, ErrNotYetValid
	}
	if err := c.validate(); err != nil {
		return Claims{}, err
	}
	return c, nil
}

func (c Claims) validate() error {
	if c.Issuer == "" || !validID.MatchString(c.Subject) || !validID.MatchString(c.Room) || c.ExpiresAt == 0 {
		return fmt.Errorf("%w: need iss, exp, and sub and room of 1-64 letters, digits, '-' or '_'", ErrInvalidClaims)
	}
	if c.NotBefore > c.ExpiresAt {
		return fmt.Errorf("%w: nbf is after exp", ErrInvalidClaims)
	}
	return nil
}

func mac(secret, data string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(data))
	return m.Sum(nil)
}
