package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// The sync wire format, matching the app's SyncWire.swift.

type syncRecord struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

type syncKey struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type syncPushRequest struct {
	Upserts []syncRecord `json:"upserts"`
	Deletes []syncKey    `json:"deletes"`
	// Accepted and ignored. The client sends where it has read up to; nothing
	// here needs it, and the response deliberately does not echo it back — see
	// the note above handleSyncPush. Kept on the wire because removing it would
	// break clients that send it.
	Cursor *string `json:"cursor"`
}

type syncPushResponse struct {
	Cursor    *string      `json:"cursor"`
	Conflicts []syncRecord `json:"conflicts"`
}

type syncPullResponse struct {
	Records []syncRecord `json:"records"`
	Deletes []syncKey    `json:"deletes"`
	Cursor  *string      `json:"cursor"`
}

// handleSyncPull returns one page of the synced records and tombstones written after the
// client's cursor, unioned across the projected tables and the generic document store,
// scoped to the authenticated account (Person).
//
// A page, not the whole delta. The client asks again from the cursor this returns and
// keeps going until it stops moving, which is the same loop it already had to run to
// tolerate a slow writer, and needs no flag on the wire: a cursor that did not advance
// means there was nothing more to advance it with.
func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	account := personIDFrom(r.Context())
	since := parseCursor(r.URL.Query().Get("since"))

	rows, err := s.store.ListSyncChangesSince(r.Context(), store.ListSyncChangesSinceParams{
		SyncAccountID: &account, Seq: &since, Lim: maxSyncPage,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	resp := syncPullResponse{Records: []syncRecord{}, Deletes: []syncKey{}}
	high := since
	weight := 0
	for i, row := range rows {
		// The first row goes in whatever it weighs. A payload bigger than the budget
		// would otherwise be skipped by every pull forever, and because the cursor only
		// advances over rows actually returned, the client would ask for it again and
		// again and never get past it. One oversized page is the lesser fault.
		if i > 0 && weight+len(row.Payload) > maxSyncPageBytes {
			break
		}
		weight += len(row.Payload)

		// Only over rows that made it into this response: the cursor is a promise that
		// everything up to it has been delivered.
		if row.Seq != nil && *row.Seq > high {
			high = *row.Seq
		}
		if row.Deleted {
			resp.Deletes = append(resp.Deletes, syncKey{Type: row.Type, ID: row.ID})
		} else {
			resp.Records = append(resp.Records, syncRecord{Type: row.Type, ID: row.ID, Payload: row.Payload})
		}
	}
	resp.Cursor = cursorString(high)
	writeJSON(w, http.StatusOK, resp)
}

// handleSyncPush applies the client's local changes. Each record is routed by
// type: projected types land in their domain table (columns projected out of the
// payload, full payload retained); everything else lands in sync_documents.
// Writes are last-write-wins within an account. A write naming a row this account
// does not own affects nothing and comes back in Conflicts, so the client learns
// its change was rejected rather than silently losing it.
//
// The response carries no cursor. Pull owns the cursor; push has no business
// touching it, and both other options are wrong:
//
// Advancing it to this push's high-water mark loses data. Every row takes a fresh
// seq from the global sequence, so a device at cursor 10 that pushes while another
// device's rows are taking seq 11 and 12 gets back 13, stores it, and pulls from
// 14 — and 11 and 12 are never delivered to it. Silent, cross-device, permanent.
//
// Echoing the client's own cursor back, which this used to do, rewinds it. The
// value is whatever the cursor was when the request was *built*, and the client
// pulls and pushes from separate tasks: a push that started at 10 and lands after
// a drain has reached 5000 writes 10 back over it. That cost a re-pull before
// pulls were paged. Now that a drain deliberately leaves the cursor behind until
// each page is delivered, it can undo an arbitrary amount of progress, and a
// chatty client can livelock a large resync.
//
// Sending nothing leaves the client's stored cursor alone — its `if let cursor`
// guard skips a null — which is the only one of the three that is always right.
func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	account := personIDFrom(r.Context())

	var req syncPushRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	if len(req.Upserts)+len(req.Deletes) > maxSyncBatch {
		writeError(w, errValidation(fmt.Sprintf(
			"a sync push carries at most %d records; split the batch", maxSyncBatch)))
		return
	}

	// Projected domain rows need an owning organization; use the caller's.
	org, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	q := s.store.WithTx(tx)

	conflicts := []syncRecord{}
	for _, rec := range req.Upserts {
		if rec.Type == "" || rec.ID == "" {
			writeError(w, errValidation("each upsert needs a type and id"))
			return
		}
		ok, err := s.applyUpsert(ctx, q, account, org.orgID, rec)
		if err != nil {
			writeError(w, syncRecordError(err, rec.Type, rec.ID))
			return
		}
		if !ok {
			conflicts = append(conflicts, rec)
		}
	}
	for _, key := range req.Deletes {
		if key.Type == "" || key.ID == "" {
			writeError(w, errValidation("each delete needs a type and id"))
			return
		}
		ok, err := s.applyDelete(ctx, q, account, key)
		if err != nil {
			writeError(w, syncRecordError(err, key.Type, key.ID))
			return
		}
		if !ok {
			conflicts = append(conflicts, syncRecord{Type: key.Type, ID: key.ID})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}

	// Cursor deliberately omitted — see the note above handleSyncPush.
	writeJSON(w, http.StatusOK, syncPushResponse{Conflicts: conflicts})
}

// applyUpsert routes one record to its projected table, or to sync_documents. It
// reports whether the write landed: an upsert against a row owned by another account
// matches nothing and returns false.
func (s *Server) applyUpsert(ctx context.Context, q *store.Queries, account, orgID uuid.UUID, rec syncRecord) (bool, error) {
	switch rec.Type {
	case "Team":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Team id must be a UUID")
		}
		var p struct {
			Name     string `json:"name"`
			AgeGroup string `json:"ageGroup"`
			Season   string `json:"season"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertTeam(ctx, store.SyncUpsertTeamParams{
			ID: id, OrganizationID: orgID, SyncAccountID: &account,
			Name: p.Name, AgeGroup: nilIfEmpty(p.AgeGroup), Season: nilIfEmpty(p.Season),
			Payload: rec.Payload,
		}))
	case "Drill":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Drill id must be a UUID")
		}
		var p struct {
			Title      string `json:"title"`
			FieldSetup string `json:"fieldSetup"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertDrill(ctx, store.SyncUpsertDrillParams{
			ID: id, OrganizationID: orgID, AuthorPersonID: &account, SyncAccountID: &account,
			Name: p.Title, Description: nilIfEmpty(p.FieldSetup), Payload: rec.Payload,
		}))
	case "Session":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Session id must be a UUID")
		}
		var p struct {
			Title     string `json:"title"`
			Objective string `json:"objective"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertSession(ctx, store.SyncUpsertSessionParams{
			ID: id, OrganizationID: orgID, AuthorPersonID: &account, SyncAccountID: &account,
			Title: p.Title, Notes: nilIfEmpty(p.Objective), Payload: rec.Payload,
		}))
	case "Person":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Person id must be a UUID")
		}
		var p struct {
			Name                  string `json:"name"`
			EmergencyContactName  string `json:"emergencyContactName"`
			EmergencyContactPhone string `json:"emergencyContactPhone"`
			MedicalNotes          string `json:"medicalNotes"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertPerson(ctx, store.SyncUpsertPersonParams{
			ID: id, SyncAccountID: &account, DisplayName: p.Name,
			EmergencyContactName:  nilIfEmpty(p.EmergencyContactName),
			EmergencyContactPhone: nilIfEmpty(p.EmergencyContactPhone),
			MedicalNotes:          nilIfEmpty(p.MedicalNotes), Payload: rec.Payload,
		}))
	case "Player":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Player id must be a UUID")
		}
		var p struct {
			PersonID string `json:"personID"`
			Name     string `json:"name"`
			Number   int32  `json:"number"`
			Position string `json:"position"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertPlayer(ctx, store.SyncUpsertPlayerParams{
			ID: id, SyncAccountID: &account, PersonID: parseUUIDPtr(p.PersonID),
			Name: nilIfEmpty(p.Name), Number: &p.Number, Position: nilIfEmpty(p.Position),
			Payload: rec.Payload,
		}))
	case "Event":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Event id must be a UUID")
		}
		var p struct {
			TeamID string `json:"teamID"`
			Title  string `json:"title"`
			Kind   string `json:"kind"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertEvent(ctx, store.SyncUpsertEventParams{
			ID: id, SyncAccountID: &account, TeamID: parseUUIDPtr(p.TeamID),
			Title: nilIfEmpty(p.Title), Kind: nilIfEmpty(p.Kind), Payload: rec.Payload,
		}))
	case "Diagram":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return false, errValidation("Diagram id must be a UUID")
		}
		var p struct {
			TeamID string `json:"teamID"`
			Title  string `json:"title"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertDiagram(ctx, store.SyncUpsertDiagramParams{
			ID: id, SyncAccountID: &account, TeamID: parseUUIDPtr(p.TeamID),
			Title: nilIfEmpty(p.Title), Payload: rec.Payload,
		}))
	default:
		return applied(q.SyncUpsertDocument(ctx, store.SyncUpsertDocumentParams{
			SyncAccountID: account, Type: rec.Type, ID: rec.ID, Payload: rec.Payload,
		}))
	}
}

// applyDelete tombstones one key in its projected table, or in sync_documents. Like
// applyUpsert it reports whether the row was actually tombstoned.
func (s *Server) applyDelete(ctx context.Context, q *store.Queries, account uuid.UUID, key syncKey) (bool, error) {
	switch key.Type {
	case "Team":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Team id must be a UUID")
		}
		return applied(q.SyncTombstoneTeam(ctx, store.SyncTombstoneTeamParams{ID: id, SyncAccountID: &account}))
	case "Drill":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Drill id must be a UUID")
		}
		return applied(q.SyncTombstoneDrill(ctx, store.SyncTombstoneDrillParams{ID: id, SyncAccountID: &account}))
	case "Session":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Session id must be a UUID")
		}
		return applied(q.SyncTombstoneSession(ctx, store.SyncTombstoneSessionParams{ID: id, SyncAccountID: &account}))
	case "Person":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Person id must be a UUID")
		}
		return applied(q.SyncTombstonePerson(ctx, store.SyncTombstonePersonParams{ID: id, SyncAccountID: &account}))
	case "Player":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Player id must be a UUID")
		}
		return applied(q.SyncTombstonePlayer(ctx, store.SyncTombstonePlayerParams{ID: id, SyncAccountID: &account}))
	case "Event":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Event id must be a UUID")
		}
		return applied(q.SyncTombstoneEvent(ctx, store.SyncTombstoneEventParams{ID: id, SyncAccountID: &account}))
	case "Diagram":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return false, errValidation("Diagram id must be a UUID")
		}
		return applied(q.SyncTombstoneDiagram(ctx, store.SyncTombstoneDiagramParams{ID: id, SyncAccountID: &account}))
	default:
		return applied(q.SyncTombstoneDocument(ctx, store.SyncTombstoneDocumentParams{
			SyncAccountID: account, Type: key.Type, ID: key.ID,
		}))
	}
}

// syncRecordError names the record a failed statement was applying, and turns the one
// failure mode the client can act on into a 400.
//
// A payload carrying a \u0000 escape is valid JSON that jsonb refuses (22P05), and it
// used to abort the push with a bare 500. That is worse than it sounds for an
// offline-first client: it retries the batch it failed to push, the batch fails
// identically every time, and that device stops syncing until the offending record is
// changed on the phone. A 400 naming the record is something the client can act on;
// everything else is still an unexplained server error, because it is one.
func syncRecordError(err error, recType, id string) error {
	if isUntranslatableCharacter(err) {
		return errValidation(fmt.Sprintf(
			"%s %s: the payload contains a character that cannot be stored (a \\u0000 escape); "+
				"remove it and push again", recType, id))
	}
	return err
}

// applied turns a sqlc :execrows result into "did this write land". Zero rows means
// the statement's ownership guard rejected it — the row belongs to another account.
func applied(rows int64, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// parseCursor turns the opaque cursor string into a seq, defaulting to 0.
func parseCursor(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func cursorString(seq int64) *string {
	s := strconv.FormatInt(seq, 10)
	return &s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseUUIDPtr returns a pointer to the parsed UUID, or nil for an empty or
// invalid string (soft references may be absent or point at un-synced entities).
func parseUUIDPtr(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}
