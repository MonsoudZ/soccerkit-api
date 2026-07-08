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
		if err := s.applyUpsert(ctx, q, account, org.orgID, rec); err != nil {
			writeError(w, err)
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
func (s *Server) applyUpsert(ctx context.Context, q *store.Queries, account, orgID uuid.UUID, rec syncRecord) error {
	switch rec.Type {
	case "Team":
		id, err := uuid.Parse(rec.ID)
		if err != nil {
			return errValidation("Team id must be a UUID")
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
			return errValidation("Drill id must be a UUID")
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
			return errValidation("Session id must be a UUID")
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
	default:
		return q.SyncUpsertDocument(ctx, store.SyncUpsertDocumentParams{
			SyncAccountID: account, Type: rec.Type, ID: rec.ID, Payload: rec.Payload,
		})
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
