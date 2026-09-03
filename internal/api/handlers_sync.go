package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	since, err := parseCursor(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, err)
		return
	}
	if since, err = s.cursorWithinSequence(r.Context(), account, since); err != nil {
		writeError(w, err)
		return
	}

	rows, err := s.store.ListSyncChangesSince(r.Context(), store.ListSyncChangesSinceParams{
		SyncAccountID: &account, Seq: &since,
		Lim: maxSyncPage, MaxBytes: maxSyncPageBytes,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	// Both page bounds are applied in the query now, including the rule that an
	// oversized first record is returned on its own. This loop used to do the byte half
	// here, which bounded the response but not the read: every row in the window had
	// already been pulled off the wire and allocated before being discarded. See
	// docs/AUDIT-5.md M1.
	resp := syncPullResponse{Records: []syncRecord{}, Deletes: []syncKey{}}
	high := since
	for _, row := range rows {
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

// cursorWithinSequence keeps a pull from confirming a read position this server could
// never have issued, and resyncs the device from the beginning when it finds one.
//
// Every cursor a pull returns is the seq of a row it actually delivered, so a
// legitimate cursor is never above sync_seq. A cursor that is above it means the
// sequence moved backwards underneath a device that had already passed it, and the
// way that happens is a database restore — which is the documented way back from
// `0009`. The device is not at fault and there is nothing for it to fix, so this is
// not an error: it is answered by rewinding it to 0 and letting the ordinary drain
// deliver the whole delta again.
//
// What it must not do is what it used to. `high` starts at the cursor the client sent
// and only rises over rows actually delivered, so an impossible cursor came back
// unchanged alongside an empty page — and an unmoved cursor is precisely the client's
// "you are up to date" condition. Every device that had synced past the restore point
// silently skipped whatever was written into the seqs it had already passed, then
// resumed normally, so nothing ever surfaced the gap. See docs/AUDIT-5.md M2.
//
// What this does not close, and it matters: the comparison can only identify a stale
// cursor while the sequence is still below it. Once enough writes land to carry
// sync_seq back over a device's cursor, that cursor is indistinguishable from a
// legitimate one, and a device reconnecting after that point still skips the window
// silently. The server cannot do better on its own — a restore rewinds every record of
// what was issued along with the data, so there is nothing left to compare against.
// The two answers that do close it are a restore-time step (bump sync_seq past the
// pre-restore high-water mark, which makes every cursor in the field valid again and
// costs nothing) and an epoch carried alongside the cursor (a wire change). Both are
// written up in docs/AUDIT-5.md M2, and the log line above names the first because it
// is the one someone can act on while reading it.
// TestSyncCannotSeeAStaleCursorOnceTheSequenceCatchesUp pins this edge.
//
// One caveat neither answer fixes, and the log line exists partly to prompt it: a
// resync from 0 delivers everything that exists, but rows the restore removed left no
// tombstone behind, so a device still holds those locally. Converging on that needs a
// reinstall or a client-side reconciliation; the server has nothing left to send.
//
// Costs one cheap read of the sequence page — no value is consumed — and only when the
// caller actually sent a cursor, so the reinstall path (since=0) does not pay for it.
func (s *Server) cursorWithinSequence(ctx context.Context, account uuid.UUID, since int64) (int64, error) {
	if since == 0 {
		return 0, nil
	}
	current, err := s.store.CurrentSyncSeq(ctx)
	if err != nil {
		return 0, err
	}
	if !current.Known || since <= current.Seq {
		return since, nil
	}
	log.Printf("sync: account %s presented cursor %d, ahead of sync_seq %d; "+
		"resyncing it from 0. A cursor above the sequence means the sequence moved "+
		"backwards under a device that had passed it — check whether this database "+
		"was restored, and if it was, bump sync_seq past the pre-restore high-water "+
		"mark, because this check stops finding stranded devices once writes carry "+
		"the sequence back over their cursors.", account, since, current.Seq)
	return 0, nil
}

// handleSyncPush applies the client's local changes. Each record is routed by
// type: projected types land in their domain table (columns projected out of the
// payload, full payload retained); everything else lands in sync_documents.
// Writes are last-write-wins within an account. A write naming a row this account
// does not own affects nothing and comes back in Conflicts, so the client learns
// its change was rejected rather than silently losing it.
//
// That conflict is also a cross-tenant existence oracle, and it is a knowing trade
// rather than an oversight (docs/AUDIT-2.md L5). A push naming a free id creates the
// row; a push naming one that exists elsewhere comes back as a conflict. The two are
// distinguishable, which tells the caller that some id exists somewhere in the database
// — the signal personVisibleTo deliberately withholds by answering 404 rather than 403.
//
// It is not fixable by softening the answer, because the answer is the feature: a client
// that is not told its write was rejected loses the write silently, which is strictly
// worse. The only real fix is to make the collision impossible, by keying the projected
// tables on (sync_account_id, id) the way sync_documents already is — a composite
// primary key on seven tables, with every foreign key that references them along for the
// trip. That is a schema migration out of all proportion to the exposure.
//
// The exposure being: the ids are client-generated UUIDv4, so 122 bits of entropy stand
// between a caller and a probe that returns anything but "created". There is nothing to
// enumerate, and a hit tells the attacker only that an id exists — not whose it is, nor
// anything in it. Written down here so the next reader can weigh it rather than
// rediscover it, and so that if these tables are ever re-keyed for another reason, this
// goes with them.
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
		if err := validateSyncKey(rec.Type, rec.ID, "upsert"); err != nil {
			writeError(w, err)
			return
		}
		if err := s.validateSyncPayload(account, rec); err != nil {
			writeError(w, err)
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
		if err := validateSyncKey(key.Type, key.ID, "delete"); err != nil {
			writeError(w, err)
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

// validateSyncKey bounds the two client-chosen strings that become part of a record's
// identity. Any type outside the seven projected ones becomes a sync_documents row
// carrying that type verbatim, and nothing bounded either string: the namespace was as
// wide as a 4 MiB body allowed. See docs/AUDIT-2.md L6.
//
// A bound and a shape, not an allowlist. The app syncs more types than this service
// projects and the rest ride as opaque documents on purpose, so an allowlist would drop
// exactly the types an older server has never heard of — turning a newer app's data into
// silent loss. The shape is permissive enough for any type name the app could reasonably
// hold (it accepts every type in the contract tests, "Prefs" and "RosterMembership"
// included) and narrow enough that a namespace cannot be filled with arbitrary text.
func validateSyncKey(recType, id, verb string) error {
	if recType == "" || id == "" {
		return errValidation("each " + verb + " needs a type and id")
	}
	if len(recType) > maxSyncTypeLen {
		return errValidation(fmt.Sprintf(
			"%s type is longer than %d characters", verb, maxSyncTypeLen))
	}
	if !plausibleSyncType(recType) {
		return errValidation(fmt.Sprintf(
			"%s type %q is not a record type name: letters, digits, underscore, dot and "+
				"dash, starting with a letter", verb, recType))
	}
	if len(id) > maxSyncIDLen {
		return errValidation(fmt.Sprintf(
			"%s %s id is longer than %d characters", verb, recType, maxSyncIDLen))
	}
	return nil
}

// plausibleSyncType reports whether a type looks like a record type name. Deliberately
// not a UUID check and not an allowlist — see validateSyncKey.
func plausibleSyncType(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9', c == '_', c == '.', c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// validateSyncPayload caps one record's payload, and records the ones approaching the
// cap so the real size distribution becomes visible.
//
// Nothing capped a single payload: the only limit was the 4 MiB body, which is also the
// reason a pull could allocate half a gigabyte to return one record — a pull's row limit
// and its byte budget only balance at ~4.2 KiB a record, and everything above that makes
// each page scan more rows than it delivers. See docs/AUDIT-5.md M1 (3).
//
// The rejection is a 400 naming the record, for the same reason syncRecordError is: an
// offline-first client retries the batch it failed to push, so a failure it cannot
// attribute to a specific record stops that device syncing with nothing to act on.
func (s *Server) validateSyncPayload(account uuid.UUID, rec syncRecord) error {
	n := len(rec.Payload)
	if n > maxSyncRecordBytes {
		return errValidation(fmt.Sprintf(
			"%s %s: payload is %d bytes, over the %d byte limit for a single record; "+
				"split it or store the large part outside sync",
			rec.Type, rec.ID, n, maxSyncRecordBytes))
	}
	if n > syncPayloadWatchBytes {
		log.Printf("sync: account %s pushed %s %s with a %d byte payload (over the %d "+
			"byte watch threshold, under the %d byte limit)",
			account, rec.Type, rec.ID, n, syncPayloadWatchBytes, maxSyncRecordBytes)
	}
	return nil
}

// projectedSyncTypes are the record types this service projects into a domain table of
// its own, with columns pulled out of the payload. Everything else rides in
// sync_documents as an opaque document, on purpose — see validateSyncKey — so this is
// the one place "the seven projected types" is written down rather than left implied by
// the case labels below.
//
// All seven key their table on a UUID, which is what lets applyUpsert and applyDelete
// parse the id once before routing rather than repeating the same four lines in every
// branch. The two must agree: a type listed here that no branch handles would otherwise
// fall through to sync_documents and split that type's records across two tables, so
// both functions end by returning an error instead of routing it anywhere.
var projectedSyncTypes = map[string]bool{
	"Team": true, "Drill": true, "Session": true, "Person": true,
	"Player": true, "Event": true, "Diagram": true,
}

// syncUUID parses the id of a projected record, naming its type in the error. The
// message says which record was rejected for the same reason syncRecordError's does: an
// offline-first client retries the batch it failed to push, so an error it cannot
// attribute to one record stops that device syncing with nothing to act on.
func syncUUID(recType, id string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, errValidation(recType + " id must be a UUID")
	}
	return parsed, nil
}

// applyUpsert routes one record to its projected table, or to sync_documents. It
// reports whether the write landed: an upsert against a row owned by another account
// matches nothing and returns false.
func (s *Server) applyUpsert(ctx context.Context, q *store.Queries, account, orgID uuid.UUID, rec syncRecord) (bool, error) {
	// Documents first: sync_documents keys on the client's string id exactly as it
	// arrives, so it is the one route that neither needs nor may be made to parse a UUID.
	if !projectedSyncTypes[rec.Type] {
		return applied(q.SyncUpsertDocument(ctx, store.SyncUpsertDocumentParams{
			SyncAccountID: account, Type: rec.Type, ID: rec.ID, Payload: rec.Payload,
		}))
	}
	id, err := syncUUID(rec.Type, rec.ID)
	if err != nil {
		return false, err
	}

	switch rec.Type {
	case "Team":
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
		var p struct {
			TeamID string `json:"teamID"`
			Title  string `json:"title"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		return applied(q.SyncUpsertDiagram(ctx, store.SyncUpsertDiagramParams{
			ID: id, SyncAccountID: &account, TeamID: parseUUIDPtr(p.TeamID),
			Title: nilIfEmpty(p.Title), Payload: rec.Payload,
		}))
	}
	return false, fmt.Errorf("sync: %q is listed as a projected type but has no upsert route", rec.Type)
}

// applyDelete tombstones one key in its projected table, or in sync_documents. Like
// applyUpsert it reports whether the row was actually tombstoned, and routes the same
// way — see projectedSyncTypes.
func (s *Server) applyDelete(ctx context.Context, q *store.Queries, account uuid.UUID, key syncKey) (bool, error) {
	if !projectedSyncTypes[key.Type] {
		return applied(q.SyncTombstoneDocument(ctx, store.SyncTombstoneDocumentParams{
			SyncAccountID: account, Type: key.Type, ID: key.ID,
		}))
	}
	id, err := syncUUID(key.Type, key.ID)
	if err != nil {
		return false, err
	}

	switch key.Type {
	case "Team":
		return applied(q.SyncTombstoneTeam(ctx, store.SyncTombstoneTeamParams{ID: id, SyncAccountID: &account}))
	case "Drill":
		return applied(q.SyncTombstoneDrill(ctx, store.SyncTombstoneDrillParams{ID: id, SyncAccountID: &account}))
	case "Session":
		return applied(q.SyncTombstoneSession(ctx, store.SyncTombstoneSessionParams{ID: id, SyncAccountID: &account}))
	case "Person":
		return applied(q.SyncTombstonePerson(ctx, store.SyncTombstonePersonParams{ID: id, SyncAccountID: &account}))
	case "Player":
		return applied(q.SyncTombstonePlayer(ctx, store.SyncTombstonePlayerParams{ID: id, SyncAccountID: &account}))
	case "Event":
		return applied(q.SyncTombstoneEvent(ctx, store.SyncTombstoneEventParams{ID: id, SyncAccountID: &account}))
	case "Diagram":
		return applied(q.SyncTombstoneDiagram(ctx, store.SyncTombstoneDiagramParams{ID: id, SyncAccountID: &account}))
	}
	return false, fmt.Errorf("sync: %q is listed as a projected type but has no delete route", key.Type)
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

// parseCursor turns the opaque cursor string into a seq.
//
// An absent cursor is 0: a device that has never synced. Anything else has to be a
// value this server could have issued, and a cursor is a sequence position, so that
// means a non-negative integer and nothing else. This used to answer garbage with a
// silent 0 — a full resync, which after the paging change is hundreds of round trips —
// so a client bug that corrupted its stored cursor showed up as an expensive pull
// rather than as an error. It is the client's own bug either way; saying so is the
// only way it gets fixed.
//
// The message deliberately does not echo the value back: it is caller-controlled and
// there is nothing in it the caller does not already know.
func parseCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errValidation("since must be a non-negative integer cursor issued by " +
			"this server, or absent to sync from the beginning")
	}
	return n, nil
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
