package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type ctxKey string

const personIDKey ctxKey = "personID"

// --- password hashing -----------------------------------------------------

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

func verifyPassword(plain, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

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

func newRefreshToken() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
	roles map[string]bool
}

func (o orgContext) hasAnyRole(roles ...string) bool {
	for _, r := range roles {
		if o.roles[r] {
			return true
		}
	}
	return false
}

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

	roles := map[string]bool{}
	found := false
	for _, m := range memberships {
		if m.OrganizationID == chosen {
			roles[m.Role] = true
			found = true
		}
	}
	if !found {
		return orgContext{}, errForbidden("you are not a member of that organization")
	}
	return orgContext{orgID: chosen, roles: roles}, nil
}
