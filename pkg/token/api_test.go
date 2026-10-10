package token

import (
	"errors"
	"testing"
	"time"
)

func TestAPITokenRoundTrip(t *testing.T) {
	tok, err := SignAPI(secret, NewAPI("key1", 10*time.Minute))
	if err != nil {
		t.Fatalf("SignAPI: %v", err)
	}
	c, err := VerifyAPI(tok, lookup, time.Now())
	if err != nil || c.Issuer != "key1" {
		t.Fatalf("VerifyAPI = %+v, %v", c, err)
	}
}

// Join tokens and API tokens cannot replace each other.
func TestTokenKindsAreNotInterchangeable(t *testing.T) {
	join, _ := Sign(secret, New("key1", "alice", "room1", time.Hour))
	if _, err := VerifyAPI(join, lookup, time.Now()); !errors.Is(err, ErrInvalidClaims) {
		t.Errorf("join token as API token: err = %v, want ErrInvalidClaims", err)
	}
	api, _ := SignAPI(secret, NewAPI("key1", time.Minute))
	if _, err := Verify(api, lookup, time.Now()); !errors.Is(err, ErrInvalidClaims) {
		t.Errorf("API token as join token: err = %v, want ErrInvalidClaims", err)
	}
}

func TestVerifyAPIRejects(t *testing.T) {
	now := time.Now()
	long := mustSignAny(t, APIClaims{Issuer: "key1", Audience: APIAudience, ExpiresAt: now.Add(3 * time.Hour).Unix()})
	expired := mustSignAny(t, APIClaims{Issuer: "key1", Audience: APIAudience, ExpiresAt: now.Add(-time.Hour).Unix()})
	wrongAud := mustSignAny(t, APIClaims{Issuer: "key1", Audience: "other", ExpiresAt: now.Add(time.Minute).Unix()})
	for name, tt := range map[string]struct {
		tok  string
		want error
	}{
		"too long":  {long, ErrInvalidClaims},
		"expired":   {expired, ErrExpired},
		"wrong aud": {wrongAud, ErrInvalidClaims},
		"garbage":   {"x.y.z", ErrMalformed},
	} {
		if _, err := VerifyAPI(tt.tok, lookup, now); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tt.want)
		}
	}
	if _, err := SignAPI(secret, APIClaims{Issuer: "key1", ExpiresAt: now.Unix()}); !errors.Is(err, ErrInvalidClaims) {
		t.Errorf("SignAPI without aud: err = %v", err)
	}
}

func mustSignAny(t *testing.T, c any) string {
	t.Helper()
	tok, err := sign(secret, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
