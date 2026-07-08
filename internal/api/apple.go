package api

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	appleIssuer  = "https://appleid.apple.com"
	appleKeysURL = "https://appleid.apple.com/auth/keys"
)

// appleIdentity is what we extract from a verified Sign in with Apple token.
type appleIdentity struct {
	Sub   string // stable Apple user id — our account key
	Email string // may be empty (user chose to hide it) or a private relay address
}

// appleVerifier validates Apple identity tokens. In production it fetches and
// caches Apple's JWKS and checks the RS256 signature, issuer, audience and
// expiry. When bypass is set it decodes the claims without verifying the
// signature — strictly for local development against a simulator.
type appleVerifier struct {
	clientID string
	bypass   bool
	client   *http.Client

	mu       sync.Mutex
	keys     map[string]*rsa.PublicKey
	fetchedA time.Time
}

func newAppleVerifier(clientID string, bypass bool) *appleVerifier {
	return &appleVerifier{
		clientID: clientID,
		bypass:   bypass,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// verify parses and validates an Apple identity token, returning the identity.
func (v *appleVerifier) verify(ctx context.Context, idToken string) (appleIdentity, error) {
	if v.bypass {
		return v.decodeUnverified(idToken)
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(appleIssuer),
		jwt.WithAudience(v.clientID),
		jwt.WithExpirationRequired(),
	)
	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keyForKID(ctx, kid)
	})
	if err != nil {
		return appleIdentity{}, fmt.Errorf("apple token invalid: %w", err)
	}
	return identityFromClaims(claims)
}

// decodeUnverified reads the claims without checking the signature (dev bypass).
func (v *appleVerifier) decodeUnverified(idToken string) (appleIdentity, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(idToken, claims); err != nil {
		return appleIdentity{}, fmt.Errorf("apple token unparseable: %w", err)
	}
	return identityFromClaims(claims)
}

func identityFromClaims(claims jwt.MapClaims) (appleIdentity, error) {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return appleIdentity{}, fmt.Errorf("apple token missing sub")
	}
	email, _ := claims["email"].(string)
	return appleIdentity{Sub: sub, Email: email}, nil
}

// keyForKID returns the public key for a key id, refreshing the JWKS cache if
// the id is unknown or the cache is stale.
func (v *appleVerifier) keyForKID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	key, ok := v.keys[kid]
	stale := time.Since(v.fetchedA) > time.Hour
	v.mu.Unlock()
	if ok && !stale {
		return key, nil
	}

	if err := v.refreshKeys(ctx); err != nil {
		// Fall back to a cached key if we still have one — a transient JWKS
		// fetch failure shouldn't reject an otherwise-valid token.
		if ok {
			return key, nil
		}
		return nil, err
	}

	v.mu.Lock()
	key, ok = v.keys[kid]
	v.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("apple signing key %q not found", kid)
	}
	return key, nil
}

func (v *appleVerifier) refreshKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, appleKeysURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch apple keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch apple keys: status %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode apple keys: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		pub, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("apple keys response contained no usable keys")
	}

	v.mu.Lock()
	v.keys = keys
	v.fetchedA = time.Now()
	v.mu.Unlock()
	return nil
}

// rsaPublicKey builds an RSA public key from the base64url modulus/exponent of a JWK.
func rsaPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
