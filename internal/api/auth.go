package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/monsoudz/soccerkit-api/internal/authz"
)

type ctxKey string

const personIDKey ctxKey = "personID"

// --- access tokens (JWT) --------------------------------------------------
// The access token identifies a Person (the authenticated human). Org and role
// are resolved per-request from memberships, not baked into the token, so a
// person who gains/loses a role or org doesn't need to re-authenticate.

func (s *Server) signAccessToken(personID uuid.UUID, email string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":   personID.String(),
		"email": email,
		"iat":   now.Unix(),
		"exp":   now.Add(s.cfg.JWTAccessTTL).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.cfg.JWTAccessSecret)
}

func (s *Server) parseAccessToken(tokenStr string) (uuid.UUID, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return s.cfg.JWTAccessSecret, nil
	})
	if err != nil || !token.Valid {
		return uuid.Nil, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return uuid.Nil, jwt.ErrTokenInvalidClaims
	}
	sub, _ := claims["sub"].(string)
	return uuid.Parse(sub)
}

// --- refresh tokens (opaque, DB-backed) -----------------------------------
// The token is 48 bytes of entropy handed to the client once. Only its SHA-256 is
// stored, so a copy of the database is not a set of working credentials. A plain hash
// is the right tool here rather than a KDF: there is no low-entropy secret to grind,
// and every refresh would pay the KDF's cost.

func newRefreshToken() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashRefreshToken maps a token to the value stored in refresh_tokens.token_hash.
func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// --- middleware -----------------------------------------------------------

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, errUnauthorized("Authentication required"))
			return
		}
		personID, err := s.parseAccessToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			writeError(w, errUnauthorized("Invalid or expired access token"))
			return
		}
		ctx := context.WithValue(r.Context(), personIDKey, personID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// personIDFrom returns the authenticated person's id set by requireAuth.
func personIDFrom(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(personIDKey).(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// orgContext resolves the organization the caller is acting in for this request
// and the roles they hold there. The org is taken from the X-Organization-ID
// header when present (and validated against membership); otherwise it falls
// back to the caller's single/first org — the common solo-coach case.
type orgContext struct {
	orgID uuid.UUID
	roles authz.Set
}

// can is the authorization question every handler asks. It is deliberately about a
// capability rather than a role name: "may you put someone on a roster" survives a
// sixth role being added, and a handler that asks by name does not.
func (o orgContext) can(c authz.Capability) bool { return o.roles.Can(c) }

// scope says how wide this caller's reads reach — the whole organization for staff,
// their own household for a parent or player. A capability check alone is not enough
// for a read: see authz.DataScope.
func (o orgContext) scope() authz.DataScope { return o.roles.Scope() }

func (s *Server) resolveOrg(r *http.Request) (orgContext, error) {
	personID := personIDFrom(r.Context())
	memberships, err := s.store.ListMembershipsForPerson(r.Context(), personID)
	if err != nil {
		return orgContext{}, err
	}
	if len(memberships) == 0 {
		return orgContext{}, errForbidden("you do not belong to any organization")
	}

	var chosen uuid.UUID
	if h := r.Header.Get("X-Organization-ID"); h != "" {
		id, perr := uuid.Parse(h)
		if perr != nil {
			return orgContext{}, errBadRequest("invalid X-Organization-ID header")
		}
		chosen = id
	} else {
		chosen = memberships[0].OrganizationID
	}

	var names []string
	for _, m := range memberships {
		if m.OrganizationID == chosen {
			names = append(names, m.Role)
		}
	}
	if names == nil {
		return orgContext{}, errForbidden("you are not a member of that organization")
	}
	return orgContext{orgID: chosen, roles: authz.NewSet(names...)}, nil
}

// requireCapability is resolveOrg plus one permission check — the opening two lines of
// almost every handler. deniedMsg is what the caller is told on a 403, so it should say
// what they cannot do, not which role they lack: role names are an implementation
// detail of the answer, and a club may hand the same capability to a different tier.
func (s *Server) requireCapability(r *http.Request, c authz.Capability, deniedMsg string) (orgContext, error) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		return orgContext{}, err
	}
	if !oc.can(c) {
		return orgContext{}, errForbidden(deniedMsg)
	}
	return oc, nil
}
