package api

import (
	"context"
	"encoding/json"
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
	Cursor  *string      `json:"cursor"`
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

// handleSyncPull returns every synced record and tombstone written after the
// client's cursor, unioned across the projected tables and the generic document
// store, scoped to the authenticated account (Person).
func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	account := personIDFrom(r.Context())
	since := parseCursor(r.URL.Query().Get("since"))

	rows, err := s.store.ListSyncChangesSince(r.Context(), store.ListSyncChangesSinceParams{
		SyncAccountID: &account, Seq: &since,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	resp := syncPullResponse{Records: []syncRecord{}, Deletes: []syncKey{}}
	high := since
	for _, row := range rows {
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
// Writes are last-write-wins; the cursor is echoed, not advanced (only a pull
// advances it), so a second device's interleaved changes are never skipped.
func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	account := personIDFrom(r.Context())

	var req syncPushRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
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

	for _, rec := range req.Upserts {
		if rec.Type == "" || rec.ID == "" {
			writeError(w, errValidation("each upsert needs a type and id"))
			return
		}
		rows, err := s.applyUpsert(ctx, q, account, org.orgID, rec)
		if err != nil {
			writeError(w, err)
			return
		}
		// Zero rows means the id exists and this account does not own it — the
		// owner guard refused the write. Reject the whole push (the tx rolls
		// back) rather than dropping the record silently. We deliberately do not
		// return the server's row: it is another account's data. A legitimate
		// client can't reach this, since it only ever pushes ids it created or
		// pulled, and a pull only ever returns rows this account owns.
		if rows == 0 {
			writeError(w, errForbidden("record is owned by another account: "+rec.Type+" "+rec.ID))
			return
		}
	}
	for _, key := range req.Deletes {
		if key.Type == "" || key.ID == "" {
			writeError(w, errValidation("each delete needs a type and id"))
			return
		}
		if err := s.applyDelete(ctx, q, account, key); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, syncPushResponse{Cursor: req.Cursor, Conflicts: []syncRecord{}})
}

// applyUpsert routes one record to its projected table, or to sync_documents.
// It returns the number of rows written: zero means the owner guard refused the
// write because the id belongs to another account (see sync.sql).
func (s *Server) applyUpsert(ctx context.Context, q *store.Queries, account, orgID uuid.UUID, rec syncRecord) (int64, error) {
	switch rec.Type {
	case "Team":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Team id must be a UUID")
		}
		var p struct {
			Name     string `json:"name"`
			AgeGroup string `json:"ageGroup"`
			Season   string `json:"season"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertTeam(ctx, store.SyncUpsertTeamParams{
			ID: id, OrganizationID: orgID, SyncAccountID: &account,
			Name: p.Name, AgeGroup: nilIfEmpty(p.AgeGroup), Season: nilIfEmpty(p.Season),
			Payload: rec.Payload,
		})
	case "Drill":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Drill id must be a UUID")
		}
		var p struct {
			Title      string `json:"title"`
			FieldSetup string `json:"fieldSetup"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertDrill(ctx, store.SyncUpsertDrillParams{
			ID: id, OrganizationID: orgID, AuthorPersonID: &account, SyncAccountID: &account,
			Name: p.Title, Description: nilIfEmpty(p.FieldSetup), Payload: rec.Payload,
		})
	case "Session":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Session id must be a UUID")
		}
		var p struct {
			Title     string `json:"title"`
			Objective string `json:"objective"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertSession(ctx, store.SyncUpsertSessionParams{
			ID: id, OrganizationID: orgID, AuthorPersonID: &account, SyncAccountID: &account,
			Title: p.Title, Notes: nilIfEmpty(p.Objective), Payload: rec.Payload,
		})
	case "Person":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Person id must be a UUID")
		}
		var p struct {
			Name                  string `json:"name"`
			EmergencyContactName  string `json:"emergencyContactName"`
			EmergencyContactPhone string `json:"emergencyContactPhone"`
			MedicalNotes          string `json:"medicalNotes"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertPerson(ctx, store.SyncUpsertPersonParams{
			ID: id, SyncAccountID: &account, DisplayName: p.Name,
			EmergencyContactName:  nilIfEmpty(p.EmergencyContactName),
			EmergencyContactPhone: nilIfEmpty(p.EmergencyContactPhone),
			MedicalNotes:          nilIfEmpty(p.MedicalNotes), Payload: rec.Payload,
		})
	case "Player":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Player id must be a UUID")
		}
		var p struct {
			PersonID string `json:"personID"`
			Name     string `json:"name"`
			Number   int32  `json:"number"`
			Position string `json:"position"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertPlayer(ctx, store.SyncUpsertPlayerParams{
			ID: id, SyncAccountID: &account, PersonID: parseUUIDPtr(p.PersonID),
			Name: nilIfEmpty(p.Name), Number: &p.Number, Position: nilIfEmpty(p.Position),
			Payload: rec.Payload,
		})
	case "Event":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Event id must be a UUID")
		}
		var p struct {
			TeamID string `json:"teamID"`
			Title  string `json:"title"`
			Kind   string `json:"kind"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertEvent(ctx, store.SyncUpsertEventParams{
			ID: id, SyncAccountID: &account, TeamID: parseUUIDPtr(p.TeamID),
			Title: nilIfEmpty(p.Title), Kind: nilIfEmpty(p.Kind), Payload: rec.Payload,
		})
	case "Diagram":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return 0, errValidation("Diagram id must be a UUID")
		}
		var p struct {
			TeamID string `json:"teamID"`
			Title  string `json:"title"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return q.SyncUpsertDiagram(ctx, store.SyncUpsertDiagramParams{
			ID: id, SyncAccountID: &account, TeamID: parseUUIDPtr(p.TeamID),
			Title: nilIfEmpty(p.Title), Payload: rec.Payload,
		})
	default:
		// sync_documents is keyed (sync_account_id, type, id), so it is scoped by
		// construction — a push can only ever touch this account's own document.
		err := q.SyncUpsertDocument(ctx, store.SyncUpsertDocumentParams{
			SyncAccountID: account, Type: rec.Type, ID: rec.ID, Payload: rec.Payload,
		})
		return 1, err
	}
}

// applyDelete tombstones one key in its projected table, or in sync_documents.
func (s *Server) applyDelete(ctx context.Context, q *store.Queries, account uuid.UUID, key syncKey) error {
	switch key.Type {
	case "Team":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Team id must be a UUID")
		}
		return q.SyncTombstoneTeam(ctx, store.SyncTombstoneTeamParams{ID: id, SyncAccountID: &account})
	case "Drill":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Drill id must be a UUID")
		}
		return q.SyncTombstoneDrill(ctx, store.SyncTombstoneDrillParams{ID: id, SyncAccountID: &account})
	case "Session":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Session id must be a UUID")
		}
		return q.SyncTombstoneSession(ctx, store.SyncTombstoneSessionParams{ID: id, SyncAccountID: &account})
	case "Person":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Person id must be a UUID")
		}
		return q.SyncTombstonePerson(ctx, store.SyncTombstonePersonParams{ID: id, SyncAccountID: &account})
	case "Player":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Player id must be a UUID")
		}
		return q.SyncTombstonePlayer(ctx, store.SyncTombstonePlayerParams{ID: id, SyncAccountID: &account})
	case "Event":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Event id must be a UUID")
		}
		return q.SyncTombstoneEvent(ctx, store.SyncTombstoneEventParams{ID: id, SyncAccountID: &account})
	case "Diagram":
		id, err := uuid.Parse(key.ID)
		if err != nil {
			return errValidation("Diagram id must be a UUID")
		}
		return q.SyncTombstoneDiagram(ctx, store.SyncTombstoneDiagramParams{ID: id, SyncAccountID: &account})
	default:
		return q.SyncTombstoneDocument(ctx, store.SyncTombstoneDocumentParams{
			SyncAccountID: account, Type: key.Type, ID: key.ID,
		})
	}
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
