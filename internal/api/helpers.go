package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// rawJSON decodes a jsonb column into a generic value for pass-through in DTOs.
func rawJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}

// pathUUID parses a UUID path parameter, returning a 400 apiError on failure.
func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, key))
	if err != nil {
		return uuid.Nil, errBadRequest("invalid " + key + " (expected a UUID)")
	}
	return id, nil
}

// parseUUIDParam parses a UUID from an arbitrary string, returning a 400 on failure.
func parseUUIDParam(v, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, errBadRequest("invalid " + name + " (expected a UUID)")
	}
	return id, nil
}

// queryInt reads an integer query param, clamped to [min,max], with a default.
func queryInt(r *http.Request, key string, def, min, max int) int32 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return int32(def)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return int32(def)
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return int32(n)
}

func queryStrPtr(r *http.Request, key string) *string {
	if v := r.URL.Query().Get(key); v != "" {
		return &v
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// parseDate parses an optional "YYYY-MM-DD" string into a pgtype.Date.
func parseDate(s *string) (pgtype.Date, error) {
	if s == nil || *s == "" {
		return pgtype.Date{Valid: false}, nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return pgtype.Date{}, errValidation("date must be in YYYY-MM-DD format")
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

func nullTimestamptz() pgtype.Timestamptz {
	return pgtype.Timestamptz{Valid: false}
}
