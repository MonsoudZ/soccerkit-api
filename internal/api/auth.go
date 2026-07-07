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

const userIDKey ctxKey = "userID"

// --- password hashing -----------------------------------------------------

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

func verifyPassword(plain, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// --- access tokens (JWT) --------------------------------------------------

func (s *Server) signAccessToken(userID uuid.UUID, email string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":   userID.String(),
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
		userID, err := s.parseAccessToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			writeError(w, errUnauthorized("Invalid or expired access token"))
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userID extracts the authenticated user id set by requireAuth.
func userIDFrom(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(userIDKey).(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}
