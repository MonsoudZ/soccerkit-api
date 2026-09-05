package api

import (
	"context"
	"log"
	"time"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Chasing a squad that has not replied.
//
// The push a fixture sends when it is scheduled is one shot, and a squad that swipes it
// away on Monday leaves the coach reading a sheet of silence on Friday night -- which is
// the state the register was built to get them out of. So as the date approaches,
// whoever still owes an answer is asked once more.
//
// Once. Not once a day, not until they reply: a reminder people learn to expect twice is
// a reminder they turn off, and the second push has already spent whatever attention the
// first one earned. The claim in ClaimGamesDueForReminder is what enforces it, and it
// enforces it across replicas rather than just within this process.
const (
	// reminderLeadTime is how far ahead a fixture gets chased. A day out is late enough
	// that an answer still reflects the weekend the player is actually looking at, and
	// early enough that a coach who is a player short can do something about it -- call
	// someone up, move a fixture, or turn up expecting nine.
	reminderLeadTime = 24 * time.Hour

	// reminderSweepInterval is how often the window is checked. It only has to be small
	// against the lead time, not against the fixture: everything inside the window is
	// claimed on the first sweep that sees it, so a coarser tick would just mean a
	// fixture scheduled at the edge is chased slightly later, never twice.
	reminderSweepInterval = 15 * time.Minute

	// sweepTimeout bounds one pass. Housekeeping must not sit on a connection while the
	// requests that people are waiting on queue behind it.
	sweepTimeout = 60 * time.Second
)

// RunReminders sweeps for unanswered fixtures on a ticker until the context is cancelled.
//
// Started by cmd/api and by nothing else, for the reason the refresh-token reaper and the
// push sender give: it is a property of the running process, not of every test that
// builds a Server. It is also started only where push is configured -- a sweep with
// nothing to deliver through would still claim every fixture it found, and a service that
// later turned push on would find its whole fixture list already marked as chased.
//
// Not run once at boot, unlike the reaper. A deploy is a bad moment to push to every
// squad with a fixture tomorrow, and a restart loop would do it repeatedly -- the claim
// would keep it to one push per fixture, but the first sweep of a fresh deploy is still
// the one worth waiting a tick for.
func (s *Server) RunReminders(ctx context.Context) {
	ticker := time.NewTicker(reminderSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCtx, cancel := context.WithTimeout(ctx, sweepTimeout)
			sent, err := s.SweepReminders(runCtx)
			cancel()
			if err != nil {
				// Logged, not fatal. This is housekeeping: a database that cannot serve
				// it has a problem the readiness probe already reports, and a sweep that
				// failed will be tried again in fifteen minutes with the same claims
				// still unclaimed.
				if ctx.Err() == nil {
					log.Printf("reminder sweep: %v", err)
				}
				continue
			}
			if sent > 0 {
				log.Printf("reminder sweep: chased %d fixture(s) starting within %s",
					sent, reminderLeadTime)
			}
		}
	}
}

// SweepReminders runs one pass and reports how many fixtures it chased.
//
// Exported because it is the unit worth testing: RunReminders is a ticker around it, and
// a test that had to wait for a tick would be testing time.Ticker.
//
// A failure on one fixture does not abandon the rest. The two claims have already been
// made by the time recipients are looked up, so returning early would leave the fixtures
// behind it marked as chased and never chased -- the one outcome this must not produce.
// The first error is carried out for the log; the sweep continues past it.
func (s *Server) SweepReminders(ctx context.Context) (int, error) {
	until := timestamptz(time.Now().Add(reminderLeadTime))
	var chased int
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	games, err := s.store.ClaimGamesDueForReminder(ctx, until)
	if err != nil {
		return 0, err
	}
	for _, game := range games {
		gameID := game.ID
		sent, err := s.remind(ctx, eventRef{
			gameID: &gameID, teamID: game.TeamID, kind: "game",
		}, func(teamName string) Notification {
			return fixtureNote(
				"Are you playing for "+teamName+"?",
				fixtureName(teamName, game.Opponent)+" is coming up and you have not replied.",
				"game", gameID, game.TeamID, timePtr(game.KickoffAt),
			)
		})
		note(err)
		if sent {
			chased++
		}
	}

	sessions, err := s.store.ClaimSessionsDueForReminder(ctx, until)
	if err != nil {
		return chased, err
	}
	for _, session := range sessions {
		// team_id is NOT NULL in the claim's WHERE, so this cannot be nil; the column is
		// nullable, which is why the generated row still says it might be.
		if session.TeamID == nil {
			continue
		}
		sessionID, teamID := session.ID, *session.TeamID
		sent, err := s.remind(ctx, eventRef{
			sessionID: &sessionID, teamID: teamID, kind: "session",
		}, func(teamName string) Notification {
			return fixtureNote(
				"Are you training with "+teamName+"?",
				session.Title+" is coming up and you have not replied.",
				"session", sessionID, teamID, timePtr(session.ScheduledAt),
			)
		})
		note(err)
		if sent {
			chased++
		}
	}
	return chased, firstErr
}

// remind pushes to whoever still owes this event an answer, and reports whether anybody
// was actually told.
//
// It does not use notifySquad: that fans out to the whole squad, which is right when a
// fixture appears or moves and wrong here. Someone who already said they are coming has
// done what was asked, and chasing them anyway is how a reminder becomes noise people
// turn off.
func (s *Server) remind(ctx context.Context, ev eventRef, build func(teamName string) Notification) (bool, error) {
	people, err := s.store.ListUnansweredReachablePeopleForEvent(ctx,
		store.ListUnansweredReachablePeopleForEventParams{
			TeamID: ev.teamID, GameID: ev.gameID, SessionID: ev.sessionID,
		})
	if err != nil {
		return false, err
	}
	if len(people) == 0 {
		// A squad that has all replied is the good case, and it costs nothing: the
		// fixture is claimed either way, so a sweep never comes back to it.
		return false, nil
	}
	team, err := s.store.GetTeam(ctx, ev.teamID)
	if err != nil {
		return false, err
	}
	note := build(team.Name)
	for _, personID := range people {
		s.notify(ctx, personID, note)
	}
	return true, nil
}

// RemindersEnabled reports whether this server can deliver a reminder at all. cmd/api
// asks before starting the loop: see RunReminders on why a sweep with nowhere to deliver
// is worse than no sweep.
func (s *Server) RemindersEnabled() bool { return s.notifier != nil }
