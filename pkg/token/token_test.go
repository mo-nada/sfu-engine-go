package token

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const secret = "0123456789abcdef0123456789abcdef"

func lookup(key string) (string, bool) {
	if key == "key1" {
		return secret, true
	}
	return "", false
}

func TestSignVerifyRoundTrip(t *testing.T) {
	c := New("key1", "alice", "room1", time.Hour)
	c.Grants.Sources = []string{"camera", "microphone"}
	tok, err := Sign(secret, c)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := Verify(tok, lookup, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "alice" || got.Room != "room1" || !got.Grants.Publish || !got.Grants.Subscribe {
		t.Errorf("claims = %+v", got)
	}
	if !got.Grants.AllowsSource("camera") || got.Grants.AllowsSource("screen") {
		t.Errorf("sources not applied: %+v", got.Grants)
	}
}

func TestVerifyRejects(t *testing.T) {
	now := time.Now()
	good, _ := Sign(secret, New("key1", "alice", "room1", time.Hour))
	parts := strings.Split(good, ".")

	expired, _ := Sign(secret, Claims{Issuer: "key1", Subject: "a", Room: "r", ExpiresAt: now.Add(-time.Hour).Unix()})
	future, _ := Sign(secret, Claims{Issuer: "key1", Subject: "a", Room: "r", NotBefore: now.Add(time.Hour).Unix(), ExpiresAt: now.Add(2 * time.Hour).Unix()})
	wrongSecret, _ := Sign("another-secret-another-secret-xx", New("key1", "alice", "room1", time.Hour))
	unknownKey, _ := Sign(secret, New("key2", "alice", "room1", time.Hour))
	noneAlg := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + "."
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"key1","sub":"mallory","room":"room1","exp":9999999999,"grants":{"publish":true}}`)) + "." + parts[2]

	tests := map[string]struct {
		tok  string
		want error
	}{
		"malformed":    {"abc", ErrMalformed},
		"bad base64":   {"!!.!!.!!", ErrMalformed},
		"expired":      {expired, ErrExpired},
		"not yet":      {future, ErrNotYetValid},
		"wrong secret": {wrongSecret, ErrSignature},
		"unknown key":  {unknownKey, ErrUnknownIssuer},
		"alg none":     {noneAlg, ErrAlgorithm},
		"tampered":     {tampered, ErrSignature},
		"too long":     {mustSign(t, Claims{Issuer: "key1", Subject: "a", Room: "r", ExpiresAt: now.Add(48 * time.Hour).Unix()}), ErrInvalidClaims},
		// nbf is within the leeway, so only the nbf-after-exp rule rejects it.
		"nbf after exp": {mustSign(t, Claims{Issuer: "key1", Subject: "a", Room: "r",
			NotBefore: now.Add(20 * time.Second).Unix(), ExpiresAt: now.Add(10 * time.Second).Unix()}), ErrInvalidClaims},
		"newline": {parts[0] + ".\n" + parts[1] + "." + parts[2], ErrMalformed},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(tt.tok, lookup, now); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestLeewayAllowsSmallClockSkew(t *testing.T) {
	tok, _ := Sign(secret, Claims{Issuer: "key1", Subject: "a", Room: "r", ExpiresAt: time.Now().Add(-10 * time.Second).Unix()})
	if _, err := Verify(tok, lookup, time.Now()); err != nil {
		t.Errorf("token 10s past expiry rejected: %v", err)
	}
}

func TestSignRejectsInvalidClaims(t *testing.T) {
	for name, c := range map[string]Claims{
		"no exp":       {Issuer: "key1", Subject: "a", Room: "r"},
		"bad identity": New("key1", "a b", "room1", time.Hour),
		"bad room":     New("key1", "alice", "room/1", time.Hour),
		"no issuer":    New("", "alice", "room1", time.Hour),
		"nbf > exp":    {Issuer: "key1", Subject: "a", Room: "r", NotBefore: 200, ExpiresAt: 100},
	} {
		if _, err := Sign(secret, c); !errors.Is(err, ErrInvalidClaims) {
			t.Errorf("%s: err = %v, want ErrInvalidClaims", name, err)
		}
	}
}

// mustSign signs c without validating it, to build tokens Sign would refuse.
func mustSign(t *testing.T, c Claims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	signing := b64.EncodeToString([]byte(header)) + "." + b64.EncodeToString(payload)
	return signing + "." + b64.EncodeToString(mac(secret, signing))
}
