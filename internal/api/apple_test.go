package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testKID = "test-key-id"

// verifierWith builds a verifier whose JWKS cache is pre-seeded with pub under
// testKID, so verify() checks signatures against it without any network fetch.
func verifierWith(clientID string, pub *rsa.PublicKey) *appleVerifier {
	v := newAppleVerifier(clientID, false)
	v.keys = map[string]*rsa.PublicKey{testKID: pub}
	v.fetchedA = time.Now() // fresh, so keyForKID never refreshes over the network
	return v
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func validClaims(clientID string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":   appleIssuer,
		"aud":   clientID,
		"sub":   "apple-user-123",
		"email": "coach@example.com",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
}

func TestAppleVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	// A second, unrelated key — tokens signed with this must be rejected when the
	// verifier only trusts `key`.
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen wrong key: %v", err)
	}

	const clientID = "com.example.SoccerCoachKit"
	ctx := context.Background()

	t.Run("accepts a valid token", func(t *testing.T) {
		v := verifierWith(clientID, &key.PublicKey)
		id, err := v.verify(ctx, signToken(t, key, validClaims(clientID)))
		if err != nil {
			t.Fatalf("expected valid token to verify, got %v", err)
		}
		if id.Sub != "apple-user-123" || id.Email != "coach@example.com" {
			t.Fatalf("unexpected identity: %+v", id)
		}
	})

	t.Run("rejects a bad signature", func(t *testing.T) {
		v := verifierWith(clientID, &key.PublicKey)
		// Correct claims, but signed by the wrong private key.
		token := signToken(t, wrongKey, validClaims(clientID))
		if _, err := v.verify(ctx, token); err == nil {
			t.Fatal("expected a bad-signature token to be rejected, got nil error")
		}
	})

	t.Run("rejects the wrong audience", func(t *testing.T) {
		v := verifierWith(clientID, &key.PublicKey)
		claims := validClaims(clientID)
		claims["aud"] = "com.someone.else"
		if _, err := v.verify(ctx, signToken(t, key, claims)); err == nil {
			t.Fatal("expected a wrong-audience token to be rejected, got nil error")
		}
	})

	t.Run("rejects the wrong issuer", func(t *testing.T) {
		v := verifierWith(clientID, &key.PublicKey)
		claims := validClaims(clientID)
		claims["iss"] = "https://evil.example.com"
		if _, err := v.verify(ctx, signToken(t, key, claims)); err == nil {
			t.Fatal("expected a wrong-issuer token to be rejected, got nil error")
		}
	})

	t.Run("rejects an expired token", func(t *testing.T) {
		v := verifierWith(clientID, &key.PublicKey)
		claims := validClaims(clientID)
		claims["exp"] = time.Now().Add(-time.Minute).Unix()
		if _, err := v.verify(ctx, signToken(t, key, claims)); err == nil {
			t.Fatal("expected an expired token to be rejected, got nil error")
		}
	})

	t.Run("bypass accepts an unverified token", func(t *testing.T) {
		// The dev-bypass path must still extract the subject without a signature.
		bypass := newAppleVerifier("", true)
		unsigned := signToken(t, wrongKey, validClaims(clientID)) // signature irrelevant here
		id, err := bypass.verify(ctx, unsigned)
		if err != nil {
			t.Fatalf("bypass should accept the token, got %v", err)
		}
		if id.Sub != "apple-user-123" {
			t.Fatalf("bypass extracted wrong sub: %q", id.Sub)
		}
	})
}
