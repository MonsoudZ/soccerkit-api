package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The wire contract with the iOS client, pinned from this side.
//
// The two repos have no linkage: soccerkit (the app) and soccerkit-api agree
// because someone kept them in step by hand, and nothing here would notice if
// they stopped. A rename on this side is silent in the worst way — Swift's
// Codable throws on a missing required key, so the coach sees "Unexpected
// server response" and their season stops syncing, while every Go test still
// passes.
//
// So these tests are written against the app's types rather than this package's.
// Every literal below is what the Swift side actually sends or requires:
//
//	AppleAuthRequest  { identityToken, authorizationCode?, fullName? }
//	AuthResponse      { token, refreshToken?, personID? }   — `token` required
//	RefreshRequest    { refreshToken }
//	RefreshResponse   { accessToken, refreshToken }          — both required
//	SyncPushRequest   { upserts, deletes, cursor? }
//	SyncPushResponse  { cursor?, conflicts }
//	SyncPullResponse  { records, deletes, cursor? }
//	SyncRecordDTO     { type, id, payload }
//	SyncKeyDTO        { type, id }
//
// See Networking/BackendAPI.swift and Networking/SyncWire.swift over there, and
// docs/sync-contract.md. If you change a name here, change it there in the same
// breath — or this fails, which is the point.

// requireKeys fails when a response is missing a key the app decodes as
// non-optional, naming the Swift type so the fix is obvious from the failure.
func requireKeys(t *testing.T, swiftType string, body map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := body[k]; !ok {
			got := make([]string, 0, len(body))
			for k := range body {
				got = append(got, k)
			}
			t.Errorf("%s requires %q; response has %v", swiftType, k, got)
		}
	}
}

func contractSignIn(t *testing.T) (token, refresh string) {
	t.Helper()
	sub := "contract-" + uuid.NewString()[:8]
	// Exactly AppleAuthRequest, including the two optionals the app sends on a
	// first authorization.
	r := do(t, http.MethodPost, "/api/v1/auth/apple", "", map[string]any{
		"identityToken":     devIdentityToken(t, sub, sub+"@example.test"),
		"authorizationCode": "auth-code",
		"fullName":          "Alex Coach",
	})
	if r.status != http.StatusOK {
		t.Fatalf("apple sign-in: status %d, body %s", r.status, r.raw)
	}
	// `token` is non-optional in Swift's AuthResponse: renaming it to
	// accessToken here (as /auth/refresh returns) breaks sign-in outright.
	requireKeys(t, "AuthResponse", r.body, "token", "refreshToken")
	tok, _ := r.body["token"].(string)
	ref, _ := r.body["refreshToken"].(string)
	if tok == "" || ref == "" {
		t.Fatalf("empty token/refreshToken: %s", r.raw)
	}
	return tok, ref
}

func TestContractAppleSignInShape(t *testing.T) {
	contractSignIn(t)
}

func TestContractRefreshShape(t *testing.T) {
	_, refresh := contractSignIn(t)
	r := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": refresh, // RefreshRequest
	})
	if r.status != http.StatusOK {
		t.Fatalf("refresh: status %d, body %s", r.status, r.raw)
	}
	// Both are non-optional in Swift's RefreshResponse. Dropping either means
	// the app can never rotate an expired session and the coach is signed out
	// mid-match with no way back but a full Sign in with Apple.
	requireKeys(t, "RefreshResponse", r.body, "accessToken", "refreshToken")
}

// An unauthenticated call must be exactly 401. The app maps only 401 to
// APIError.unauthorized, which is what triggers refresh-and-retry; any other
// status surfaces as a plain failure and sync stops until the next launch.
func TestContractUnauthorizedIs401(t *testing.T) {
	r := do(t, http.MethodGet, "/api/v1/sync", "not-a-real-token", nil)
	if r.status != http.StatusUnauthorized {
		t.Fatalf("expected 401 so the app refreshes and retries, got %d", r.status)
	}
}

func TestContractSyncShapes(t *testing.T) {
	token, _ := contractSignIn(t)

	r := do(t, http.MethodGet, "/api/v1/sync", token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("pull: status %d, body %s", r.status, r.raw)
	}
	requireKeys(t, "SyncPullResponse", r.body, "records", "deletes")

	teamID := uuid.NewString()
	r = do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{map[string]any{
			"type": "Team", "id": teamID,
			"payload": map[string]any{"id": teamID, "name": "Northside Falcons"},
		}},
		"deletes": []any{},
		"cursor":  nil,
	})
	if r.status != http.StatusOK {
		t.Fatalf("push: status %d, body %s", r.status, r.raw)
	}
	requireKeys(t, "SyncPushResponse", r.body, "cursor", "conflicts")
}

// A push must not hand back a cursor.
//
// The client stores whatever a push returns — apply(..., cursor: response.cursor)
// then `if let cursor { defaults.set(...) }` — so anything here becomes its read
// position. Both non-nil options are wrong.
//
// Advancing to this push's high-water mark skips a second device's interleaved
// rows for good: seqs come from one global sequence, so a device at 10 pushing
// alongside another device's 11 and 12 would be told 13 and never see 11 or 12.
//
// Echoing the request's cursor, which this used to do, rewinds. That value is
// from when the request was built, and the client pushes and pulls in separate
// tasks: a push that began at 10 and lands after a drain reached 5000 writes 10
// back. With paged pulls that undoes real progress rather than costing one
// re-pull.
func TestContractPushReturnsNoCursor(t *testing.T) {
	token, _ := contractSignIn(t)

	// Read up to a real position first, so an echo would be visible as one.
	drillID := uuid.NewString()
	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{map[string]any{
			"type": "Drill", "id": drillID,
			"payload": map[string]any{"id": drillID, "title": "Rondo"},
		}},
		"deletes": []any{}, "cursor": nil,
	}); r.status != http.StatusOK {
		t.Fatalf("seed push: status %d, body %s", r.status, r.raw)
	}
	pullRecords(t, token) // advances a real client's cursor well past "7"

	// A push that reports an old read position, the way one built before a drain
	// does.
	r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{}, "deletes": []any{}, "cursor": "7",
	})
	if r.status != http.StatusOK {
		t.Fatalf("push: status %d, body %s", r.status, r.raw)
	}
	if _, present := r.body["cursor"]; !present {
		t.Fatal("SyncPushResponse requires a cursor key, even when null")
	}
	if got := r.body["cursor"]; got != nil {
		t.Errorf("push returned cursor %v; it must return null so the client's "+
			"read position is left alone", got)
	}
}

// A record type this server doesn't project must still round-trip. The app syncs
// sixteen types and this server lifts seven; the rest ride as opaque documents,
// and an older server must not drop a newer app's records on the floor.
func TestContractUnprojectedTypesRoundTrip(t *testing.T) {
	token, _ := contractSignIn(t)

	ids := map[string]string{}
	upserts := []any{}
	for _, typ := range []string{"RosterMembership", "OrgMembership", "ShareGrant", "FormInstance"} {
		id := uuid.NewString()
		ids[typ] = id
		upserts = append(upserts, map[string]any{
			"type": typ, "id": id,
			"payload": map[string]any{"id": id, "marker": typ},
		})
	}
	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": upserts, "deletes": []any{}, "cursor": nil,
	}); r.status != http.StatusOK {
		t.Fatalf("push: status %d, body %s", r.status, r.raw)
	}

	got := pullRecords(t, token)
	for typ, id := range ids {
		rec, ok := got[typ]
		if !ok {
			t.Errorf("%s did not come back; an unprojected type must not be dropped", typ)
			continue
		}
		if rec["id"] != id {
			t.Errorf("%s: id %v, want %v", typ, rec["id"], id)
		}
		payload, _ := rec["payload"].(map[string]any)
		if payload["marker"] != typ {
			t.Errorf("%s: payload not preserved: %v", typ, payload)
		}
	}
}

// The singleton prefs record has the fixed id "prefs", not a UUID. Anything that
// starts parsing record ids as UUIDs unconditionally breaks it.
func TestContractPrefsRecordHasANonUUIDID(t *testing.T) {
	token, _ := contractSignIn(t)
	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{map[string]any{
			"type": "Prefs", "id": "prefs",
			"payload": map[string]any{"selectedTeamID": uuid.NewString()},
		}},
		"deletes": []any{}, "cursor": nil,
	}); r.status != http.StatusOK {
		t.Fatalf("a non-UUID record id must be accepted: status %d, body %s", r.status, r.raw)
	}
	if _, ok := pullRecords(t, token)["Prefs"]; !ok {
		t.Error("the prefs record did not come back")
	}
}

// Swift's JSONEncoder writes Date as a Double — seconds since 2001-01-01 — and
// payloads are opaque here, so the number must come back a number. Reformatting
// it as an RFC 3339 string (the reflex when a field is called `date`) would make
// every game and session undecodable on the app.
func TestContractSwiftDatesSurviveAsNumbers(t *testing.T) {
	token, _ := contractSignIn(t)
	const swiftDate = 778000000.5 // fractional on purpose: seconds, not an integer
	gameID := uuid.NewString()

	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{map[string]any{
			"type": "Game", "id": gameID,
			"payload": map[string]any{"id": gameID, "opponent": "Rivertown", "date": swiftDate},
		}},
		"deletes": []any{}, "cursor": nil,
	}); r.status != http.StatusOK {
		t.Fatalf("push: status %d, body %s", r.status, r.raw)
	}

	rec, ok := pullRecords(t, token)["Game"]
	if !ok {
		t.Fatal("the game did not come back")
	}
	payload, _ := rec["payload"].(map[string]any)
	switch got := payload["date"].(type) {
	case float64:
		if got != swiftDate {
			t.Errorf("date %v, want %v — the value must survive exactly", got, swiftDate)
		}
	default:
		t.Errorf("date came back as %T (%v); Swift decodes it as a Double", got, got)
	}
}

// A tombstone is { type, id } — SyncKeyDTO — and must actually remove the record.
func TestContractDeletesUseTypeAndID(t *testing.T) {
	token, _ := contractSignIn(t)
	drillID := uuid.NewString()

	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{map[string]any{
			"type": "Drill", "id": drillID,
			"payload": map[string]any{"id": drillID, "title": "Rondo"},
		}},
		"deletes": []any{}, "cursor": nil,
	}); r.status != http.StatusOK {
		t.Fatalf("push: status %d, body %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{},
		"deletes": []any{map[string]any{"type": "Drill", "id": drillID}},
		"cursor":  nil,
	}); r.status != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", r.status, r.raw)
	}

	if _, still := pullRecords(t, token)["Drill"]; still {
		t.Error("a tombstoned drill still came back as a record")
	}
}

// Account deletion is DELETE /v1/me with no body. The app treats any 2xx as
// success and only then wipes local data, so a non-2xx here is what stops it
// claiming an account was deleted while the server still holds it.
func TestContractDeleteMeReturns2xx(t *testing.T) {
	token, _ := contractSignIn(t)
	r := do(t, http.MethodDelete, "/api/v1/me", token, nil)
	if r.status < 200 || r.status >= 300 {
		t.Fatalf("DELETE /v1/me: status %d, body %s", r.status, r.raw)
	}
}

// pullRecords reads every record the account can see, keyed by type. It follows
// the cursor the way the app does, so it keeps working once pulls are paged.
func pullRecords(t *testing.T, token string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	cursor := "0"
	for page := 0; page < 100; page++ {
		r := do(t, http.MethodGet, fmt.Sprintf("/api/v1/sync?since=%s", cursor), token, nil)
		if r.status != http.StatusOK {
			t.Fatalf("pull: status %d, body %s", r.status, r.raw)
		}
		var body struct {
			Records []map[string]any `json:"records"`
			Cursor  *string          `json:"cursor"`
		}
		if err := json.Unmarshal(r.raw, &body); err != nil {
			t.Fatalf("decode pull: %v", err)
		}
		for _, rec := range body.Records {
			if typ, ok := rec["type"].(string); ok {
				out[typ] = rec
			}
		}
		if body.Cursor == nil || *body.Cursor == cursor || len(body.Records) == 0 {
			break
		}
		cursor = *body.Cursor
	}
	return out
}
