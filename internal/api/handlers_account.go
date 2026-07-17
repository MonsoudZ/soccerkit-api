package api

import (
	"net/http"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// handleDeleteMe erases the caller's account and everything it owns, in one
// transaction. This is the server half of the iOS "Delete Account" flow and an
// App Store requirement for any app that offers account creation.
//
// It returns 204 and is idempotent: a retry after a flaky network — or a second
// delete of an already-gone account — returns 204, never 500. The access token
// (a JWT) outlives the row it points at, so a call whose Person is already gone
// must succeed too; every DELETE below is a no-op on rows that are already
// absent, and ListPersonalOrgIDsForPerson simply returns nothing.
//
// The cascade has three moving parts, run in order:
//
//  1. Orphaned athletes — Persons whose ONLY org linkage is the caller's org.
//     Deleting the org strips their membership/roster rows but leaves the Person
//     itself (name, birthdate, medical notes — minors' PII we are legally
//     required to erase). ON DELETE CASCADE will not reach them, so they are
//     deleted explicitly. See SelectOrphanedAthletePersonIDs for the multi-org
//     guard that spares a shared athlete still rostered under another coach.
//  2. The caller's personal org(s) — cascades teams, drills, sessions,
//     templates, games, roster memberships and share-grants.
//  3. The caller's own Person — cascades their user_account, refresh tokens,
//     memberships, guardianships, submitted form instances, and every
//     sync-owned row (players/events/diagrams/sync_documents and any
//     sync-created athlete Persons — all keyed by sync_account_id).
func (s *Server) handleDeleteMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID := personIDFrom(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	q := s.store.WithTx(tx)

	orgIDs, err := q.ListPersonalOrgIDsForPerson(ctx, callerID)
	if err != nil {
		writeError(w, err)
		return
	}

	athleteIDs, err := q.SelectOrphanedAthletePersonIDs(ctx, store.SelectOrphanedAthletePersonIDsParams{
		OrgIds:         orgIDs,
		CallerPersonID: callerID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if len(athleteIDs) > 0 {
		if err := q.DeletePersonsByIDs(ctx, athleteIDs); err != nil {
			writeError(w, err)
			return
		}
	}
	if len(orgIDs) > 0 {
		if err := q.DeleteOrganizationsByIDs(ctx, orgIDs); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := q.DeletePersonByID(ctx, callerID); err != nil {
		writeError(w, err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
