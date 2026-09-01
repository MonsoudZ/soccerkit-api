package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint (23505) error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isUntranslatableCharacter reports whether err is Postgres 22P05, which jsonb raises
// for a character it cannot store as text — in practice a \u0000 escape. That is legal
// JSON and illegal in the column, so it is the caller's input that is wrong, not the
// server.
func isUntranslatableCharacter(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P05"
}

// apiError is a typed HTTP error rendered as a consistent JSON envelope.
type apiError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func errBadRequest(msg string) *apiError { return &apiError{http.StatusBadRequest, "BAD_REQUEST", msg} }
func errValidation(msg string) *apiError {
	return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", msg}
}
func errUnauthorized(msg string) *apiError {
	return &apiError{http.StatusUnauthorized, "UNAUTHORIZED", msg}
}
func errForbidden(msg string) *apiError { return &apiError{http.StatusForbidden, "FORBIDDEN", msg} }
func errNotFound(msg string) *apiError  { return &apiError{http.StatusNotFound, "NOT_FOUND", msg} }
func errConflict(msg string) *apiError  { return &apiError{http.StatusConflict, "CONFLICT", msg} }

// errEmailAlreadyRegistered is what /auth/apple answers when the identity's address
// already has an account. It carries its own code rather than a bare CONFLICT because
// the client has to act on it: this one means "sign in with your password, then link
// Apple from settings", while the other 409 on that endpoint (a pre-claimed Person id)
// means "contact support". A status code alone cannot tell them apart.
func errEmailAlreadyRegistered() *apiError {
	return &apiError{http.StatusConflict, "EMAIL_ALREADY_REGISTERED",
		"An account already exists for this email address. Sign in with your password, " +
			"then link Sign in with Apple from your account settings."}
}

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError renders an error as the standard envelope. Known *apiError values
// keep their status/code; anything else becomes a 500.
func writeError(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.status, map[string]any{"error": apiErr})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": map[string]string{"code": "INTERNAL", "message": "An unexpected error occurred."},
	})
}

// decodeJSON reads and strictly decodes a JSON request body into dst.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadRequest("invalid JSON body: " + err.Error())
	}
	return nil
}
