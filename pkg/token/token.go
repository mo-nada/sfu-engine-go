// Package token creates and verifies SFU join tokens and API tokens.
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
//
// An API token authenticates a tenant's backend to the server API (/v1). It
// is a JWT signed the same way, with claims iss (API key), aud "sfu-api",
// iat and exp (at most MaxAPITTL ahead). Join and API tokens cannot be used
// in place of each other.
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
	return sign(secret, c)
}

// Verify checks a join token's algorithm, signature and validity window at
// now. secretFor returns the secret for an API key, or false if the key is
// unknown.
func Verify(tok string, secretFor func(apiKey string) (string, bool), now time.Time) (Claims, error) {
	payload, err := verifySignature(tok, secretFor)
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if err := checkTimes(c.NotBefore, c.ExpiresAt, MaxTTL, now); err != nil {
		return Claims{}, err
	}
	if err := c.validate(); err != nil {
		return Claims{}, err
	}
	return c, nil
}

// verifySignature checks the format, the algorithm and the HMAC, and returns
// the decoded payload. The secret is looked up by the payload's "iss".
func verifySignature(tok string, secretFor func(apiKey string) (string, bool)) ([]byte, error) {
	parts := strings.Split(tok, ".")
	// The decoder skips '\r' and '\n'; reject them so encodings stay unique.
	if len(parts) != 3 || strings.ContainsAny(tok, "\r\n") {
		return nil, ErrMalformed
	}
	rawHeader, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return nil, ErrMalformed
	}
	// Only HS256: rejects "none" and algorithm-confusion attacks.
	if h.Alg != "HS256" {
		return nil, ErrAlgorithm
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var iss struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &iss); err != nil {
		return nil, ErrMalformed
	}
	secret, ok := secretFor(iss.Issuer)
	if !ok {
		return nil, ErrUnknownIssuer
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	if !hmac.Equal(sig, mac(secret, parts[0]+"."+parts[1])) {
		return nil, ErrSignature
	}
	return payload, nil
}

// checkTimes checks exp (required, not past, at most maxTTL ahead) and nbf.
func checkTimes(nbf, exp int64, maxTTL time.Duration, now time.Time) error {
	if exp == 0 || now.After(time.Unix(exp, 0).Add(Leeway)) {
		return ErrExpired
	}
	if time.Unix(exp, 0).After(now.Add(maxTTL + Leeway)) {
		return fmt.Errorf("%w: expires more than %s from now", ErrInvalidClaims, maxTTL)
	}
	if nbf != 0 && now.Add(Leeway).Before(time.Unix(nbf, 0)) {
		return ErrNotYetValid
	}
	return nil
}

// sign encodes and signs any claims value.
func sign(secret string, claims any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString([]byte(header)) + "." + b64.EncodeToString(payload)
	return signing + "." + b64.EncodeToString(mac(secret, signing)), nil
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
