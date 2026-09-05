package api

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// PATCH decoding.
//
// Absence and null mean different things in a PATCH: a field that is not in the body is
// untouched, and a field set to null is cleared. json.Unmarshal into a typed struct
// cannot tell those apart -- null decodes as the zero value and so does absence -- so
// every PATCH body here is read one raw key at a time, and each key is decoded into its
// own target only after it has been seen.
//
// The functions below are shared by every PATCH endpoint. They were written for
// PATCH /games/{id} and lived in handlers_games.go until teams, persons and
// organizations needed the same three behaviours.

// decodePatch reads a PATCH body as raw keys and rejects any key outside the allowed
// set.
//
// The rejection is not optional politeness. decodeJSON asks for DisallowUnknownFields,
// but that is a no-op against a map, so without this check a misspelled field would be
// silently dropped and the endpoint would answer 200 for a change it never made. Every
// other endpoint in this API decodes strictly; this keeps PATCH honest too.
func decodePatch(r *http.Request, allowed map[string]bool) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		return nil, err
	}
	for key := range raw {
		if !allowed[key] {
			return nil, errValidation("unknown field: " + key)
		}
	}
	return raw, nil
}

// requiredString decodes a JSON string for a column that cannot be null, rejecting an
// explicit null and an empty string along with any other type.
func requiredString(raw json.RawMessage, field string) (string, error) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", errValidation(field + " must be a string")
	}
	if v == "" {
		return "", errValidation(field + " cannot be empty")
	}
	return v, nil
}

// optionalString decodes a JSON string or an explicit null, rejecting any other type.
// Null is meaningful here: it is how a caller clears a nullable column.
func optionalString(raw json.RawMessage, field string) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errValidation(field + " must be a string or null")
	}
	return &v, nil
}

// optionalInt32 decodes a JSON integer or an explicit null, rejecting any other type
// and any number with a fractional part.
func optionalInt32(raw json.RawMessage, field string) (*int32, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil || v != math.Trunc(v) {
		return nil, errValidation(field + " must be a whole number or null")
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return nil, errValidation(field + " is out of range")
	}
	n := int32(v)
	return &n, nil
}

// optionalTimestamptz decodes an RFC3339 string or an explicit null, for the columns that
// hold an instant rather than a day: a game's kickoff, a training session's start.
//
// Null is the point. It used to be json.Unmarshal straight into a string, where null is a
// silent no-op that leaves "" behind and then fails RFC3339 parsing, so a cancelled
// fixture's kickoff could not be unset (docs/AUDIT-2.md L3). That fix lived inline in
// PATCH /games/{id} until sessions needed the same three behaviours -- which is the same
// path the rest of this file took.
func optionalTimestamptz(raw json.RawMessage, field string) (pgtype.Timestamptz, error) {
	if string(raw) == "null" {
		return pgtype.Timestamptz{Valid: false}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return pgtype.Timestamptz{}, errValidation(field + " must be an RFC3339 timestamp or null")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return pgtype.Timestamptz{}, errValidation(field + " must be an RFC3339 timestamp or null")
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, nil
}

// optionalDate decodes a "YYYY-MM-DD" string or an explicit null, matching the format
// parseDate accepts on the create paths.
func optionalDate(raw json.RawMessage, field string) (pgtype.Date, error) {
	if string(raw) == "null" {
		return pgtype.Date{Valid: false}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return pgtype.Date{}, errValidation(field + " must be a YYYY-MM-DD string or null")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return pgtype.Date{}, errValidation(field + " must be in YYYY-MM-DD format")
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

// syncPatch collects the fields of an edit that belong to the sync contract, keyed the
// way the app's own record keys them, so they can be merged into the row's payload.
//
// It exists because a REST edit has to be written twice to be seen: once into the
// projected columns this API reads, and once into the payload a sync pull returns. See
// the note on UpdateTeam in db/queries/teams.sql for why a column-only edit is silently
// lost. Fields outside the contract never go in here -- the server would be inventing
// keys the app does not know.
type syncPatch map[string]any

// set records one projected field.
func (p syncPatch) set(key string, value any) { p[key] = value }

// syncString renders a nullable column the way the app's own record spells it.
//
// Not as JSON null. Every projected upsert runs its payload strings through nilIfEmpty,
// which turns "" into a NULL column -- so the app writes "" where this API stores NULL,
// and "" is the faithful inverse. It is also the safe one: "" decodes into a Swift
// String whether the app declared that field optional or not, while null decodes into
// only the optional case and would fail the record outright otherwise.
func syncString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// marshal renders the patch for the payload_patch parameter, and reports whether there
// is anything to merge. An empty patch merges nothing and the query skips the write.
func (p syncPatch) marshal() ([]byte, bool) {
	if len(p) == 0 {
		return []byte("{}"), false
	}
	b, err := json.Marshal(map[string]any(p))
	if err != nil {
		// Every value here came off a decoded request body, so this cannot fail; if it
		// somehow did, merging nothing is the safe answer rather than a corrupt payload.
		return []byte("{}"), false
	}
	return b, true
}
