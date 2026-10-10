package token

import (
	"encoding/json"
	"fmt"
	"time"
)

// APIAudience is the "aud" of every API token.
const APIAudience = "sfu-api"

// MaxAPITTL is the longest accepted remaining lifetime of an API token.
const MaxAPITTL = time.Hour

// APIClaims is the payload of an API token.
type APIClaims struct {
	Issuer    string `json:"iss"` // tenant API key
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

// NewAPI returns API token claims for apiKey, valid for ttl from now.
func NewAPI(apiKey string, ttl time.Duration) APIClaims {
	now := time.Now()
	return APIClaims{Issuer: apiKey, Audience: APIAudience, IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()}
}

// SignAPI returns the signed API token for c.
func SignAPI(secret string, c APIClaims) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	return sign(secret, c)
}

// VerifyAPI checks an API token like Verify checks a join token, and also
// requires aud to be APIAudience, so a join token is never accepted here.
func VerifyAPI(tok string, secretFor func(apiKey string) (string, bool), now time.Time) (APIClaims, error) {
	payload, err := verifySignature(tok, secretFor)
	if err != nil {
		return APIClaims{}, err
	}
	var c APIClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return APIClaims{}, ErrMalformed
	}
	if err := checkTimes(0, c.ExpiresAt, MaxAPITTL, now); err != nil {
		return APIClaims{}, err
	}
	if err := c.validate(); err != nil {
		return APIClaims{}, err
	}
	return c, nil
}

func (c APIClaims) validate() error {
	if c.Issuer == "" || c.Audience != APIAudience || c.ExpiresAt == 0 {
		return fmt.Errorf("%w: API token needs iss, exp and aud %q", ErrInvalidClaims, APIAudience)
	}
	return nil
}
