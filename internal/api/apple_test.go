package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
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
	v.fetchedAt = time.Now() // fresh, so keyForKID never refreshes over the network
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

// TestUnknownKIDDoesNotRefetchWhileCooling — the JWKS cache only ever holds keys Apple
// returned, so a token carrying an arbitrary kid missed every time and forced an
// outbound request. /auth/apple is unauthenticated and the header is caller-controlled,
// which made this service an amplifier for requests to Apple.
func TestUnknownKIDDoesNotRefetchWhileCooling(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	v := verifierWith("com.example.app", &key.PublicKey)
	// Pretend a refresh was just attempted, as one would have been on the first miss.
	v.attemptedAt = time.Now()
	// Point the fetcher at a dead address, so any real attempt is unmistakable.
	v.client = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("keyForKID went to the network for an unknown kid inside the cooldown")
			return nil, errors.New("no network in this test")
		}),
		Timeout: time.Second,
	}

	if _, err := v.keyForKID(context.Background(), "some-kid-we-have-never-seen"); err == nil {
		t.Error("an unknown kid should be an error, not a silent pass")
	}
	// A kid we do hold still resolves from cache.
	if _, err := v.keyForKID(context.Background(), testKID); err != nil {
		t.Errorf("a cached kid should still resolve: %v", err)
	}
}

// TestRefreshKeysStampsAttemptEvenOnFailure — the cooldown bounds outbound requests, so
// a failing fetch must not be retryable any faster than a succeeding one.
func TestRefreshKeysStampsAttemptEvenOnFailure(t *testing.T) {
	v := newAppleVerifier("com.example.app", false)
	var calls int
	v.client = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("apple is unreachable")
		}),
		Timeout: time.Second,
	}

	if err := v.refreshKeys(context.Background()); err == nil {
		t.Fatal("expected the failing fetch to error")
	}
	if v.attemptedAt.IsZero() {
		t.Fatal("a failed attempt must still be stamped, or the cooldown never applies")
	}
	// The next miss is inside the cooldown, so it must not reach the network again.
	if _, err := v.keyForKID(context.Background(), "another-kid"); err == nil {
		t.Error("expected an error for an uncached kid")
	}
	if calls != 1 {
		t.Errorf("network attempts = %d, want 1", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
