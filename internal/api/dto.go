package api

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

func rfc3339(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format(time.RFC3339)
}

// timePtr is rfc3339 for a column that is genuinely optional, where "" and "never" are
// different answers a client has to be able to tell apart.
func timePtr(ts pgtype.Timestamptz) *string {
	if !ts.Valid {
		return nil
	}
	s := rfc3339(ts)
	return &s
}

func dateStr(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.UTC().Format("2006-01-02")
	return &s
}

// ---- identity ------------------------------------------------------------

type Person struct {
	ID                    uuid.UUID `json:"id"`
	DisplayName           string    `json:"displayName"`
	GivenName             *string   `json:"givenName"`
	FamilyName            *string   `json:"familyName"`
	Birthdate             *string   `json:"birthdate"`
	Email                 *string   `json:"email"`
	Phone                 *string   `json:"phone"`
	EmergencyContactName  *string   `json:"emergencyContactName"`
	EmergencyContactPhone *string   `json:"emergencyContactPhone"`
	MedicalNotes          *string   `json:"medicalNotes"`
	CreatedAt             string    `json:"createdAt"`
}

func personDTO(p store.Person) Person {
	return Person{
		ID: p.ID, DisplayName: p.DisplayName, GivenName: p.GivenName, FamilyName: p.FamilyName,
		Birthdate: dateStr(p.Birthdate), Email: p.Email, Phone: p.Phone,
		EmergencyContactName: p.EmergencyContactName, EmergencyContactPhone: p.EmergencyContactPhone,
		MedicalNotes: p.MedicalNotes, CreatedAt: rfc3339(p.CreatedAt),
	}
}

type MembershipView struct {
	OrganizationID   uuid.UUID `json:"organizationId"`
	OrganizationName string    `json:"organizationName"`
	OrganizationKind string    `json:"organizationKind"`
	Role             string    `json:"role"`
}

// Me bundles the authenticated person with their memberships (orgs + roles).
type Me struct {
	Person      Person           `json:"person"`
	Memberships []MembershipView `json:"memberships"`
}

type AuthResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Me           Me     `json:"me"`
}

// ---- teams & roster ------------------------------------------------------

type Team struct {
	ID                uuid.UUID `json:"id"`
	OrganizationID    uuid.UUID `json:"organizationId"`
	Name              string    `json:"name"`
	AgeGroup          *string   `json:"ageGroup"`
	Season            *string   `json:"season"`
	ActiveRosterCount int64     `json:"activeRosterCount"`
	CreatedAt         string    `json:"createdAt"`
}

func teamDTO(t store.Team, activeRoster int64) Team {
	return Team{
		ID: t.ID, OrganizationID: t.OrganizationID, Name: t.Name, AgeGroup: t.AgeGroup,
		Season: t.Season, ActiveRosterCount: activeRoster, CreatedAt: rfc3339(t.CreatedAt),
	}
}

type RosterEntry struct {
	ID           uuid.UUID `json:"id"`
	PersonID     uuid.UUID `json:"personId"`
	DisplayName  string    `json:"displayName"`
	Email        *string   `json:"email"`
	Birthdate    *string   `json:"birthdate"`
	JerseyNumber *int32    `json:"jerseyNumber"`
	Position     *string   `json:"position"`
	JoinedOn     *string   `json:"joinedOn"`
	Status       string    `json:"status"`
}

func rosterRowDTO(r store.ListActiveRosterRow) RosterEntry {
	return RosterEntry{
		ID: r.ID, PersonID: r.PersonID, DisplayName: r.DisplayName, Email: r.Email,
		Birthdate: dateStr(r.Birthdate), JerseyNumber: r.JerseyNumber, Position: r.Position,
		JoinedOn: dateStr(r.JoinedOn), Status: r.Status,
	}
}

// ---- evaluation engine ---------------------------------------------------

type FormField struct {
	ID       uuid.UUID `json:"id"`
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Kind     string    `json:"kind"`
	Position int32     `json:"position"`
	Config   any       `json:"config,omitempty"`
}

func fieldDTO(f store.FormField) FormField {
	return FormField{
		ID: f.ID, Key: f.Key, Label: f.Label, Kind: f.Kind, Position: f.Position,
		Config: rawJSON(f.Config),
	}
}

type FormTemplate struct {
	ID             uuid.UUID   `json:"id"`
	OrganizationID *uuid.UUID  `json:"organizationId"`
	Context        string      `json:"context"`
	Name           string      `json:"name"`
	SubjectType    string      `json:"subjectType"`
	Version        int32       `json:"version"`
	IsSeed         bool        `json:"isSeed"`
	Fields         []FormField `json:"fields,omitempty"`
}

func templateDTO(t store.FormTemplate, fields []FormField) FormTemplate {
	return FormTemplate{
		ID: t.ID, OrganizationID: t.OrganizationID, Context: t.Context, Name: t.Name,
		SubjectType: t.SubjectType, Version: t.Version, IsSeed: t.IsSeed, Fields: fields,
	}
}

type Answer struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Kind         string   `json:"kind"`
	NumericValue *float64 `json:"numericValue"`
	BoolValue    *bool    `json:"boolValue"`
	TextValue    *string  `json:"textValue"`
}

func answerRowDTO(a store.ListAnswersForInstanceRow) Answer {
	return Answer{
		Key: a.Key, Label: a.Label, Kind: a.Kind,
		NumericValue: a.NumericValue, BoolValue: a.BoolValue, TextValue: a.TextValue,
	}
}

type FormInstance struct {
	ID              uuid.UUID  `json:"id"`
	TemplateID      uuid.UUID  `json:"templateId"`
	Context         string     `json:"context"`
	SubjectPersonID *uuid.UUID `json:"subjectPersonId"`
	SubjectTeamID   *uuid.UUID `json:"subjectTeamId"`
	ContextRefType  *string    `json:"contextRefType"`
	ContextRefID    *uuid.UUID `json:"contextRefId"`
	SubmittedAt     string     `json:"submittedAt"`
	Answers         []Answer   `json:"answers,omitempty"`
}

type InstanceSummary struct {
	ID             uuid.UUID  `json:"id"`
	TemplateID     uuid.UUID  `json:"templateId"`
	Context        string     `json:"context"`
	TemplateName   string     `json:"templateName"`
	ContextRefType *string    `json:"contextRefType"`
	ContextRefID   *uuid.UUID `json:"contextRefId"`
	SubmittedAt    string     `json:"submittedAt"`
}

func instanceSummaryDTO(r store.ListInstancesForPersonRow) InstanceSummary {
	return InstanceSummary{
		ID: r.ID, TemplateID: r.TemplateID, Context: r.Context, TemplateName: r.TemplateName,
		ContextRefType: r.ContextRefType, ContextRefID: r.ContextRefID, SubmittedAt: rfc3339(r.SubmittedAt),
	}
}

// ---- content & game day --------------------------------------------------

type Drill struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	CreatedAt   string    `json:"createdAt"`
}

func drillDTO(d store.Drill) Drill {
	return Drill{ID: d.ID, Name: d.Name, Description: d.Description, CreatedAt: rfc3339(d.CreatedAt)}
}

type SessionBlock struct {
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	DrillID     *uuid.UUID `json:"drillId"`
	DrillName   *string    `json:"drillName"`
	DurationMin *int32     `json:"durationMin"`
	Position    int32      `json:"position"`
	Notes       *string    `json:"notes"`
}

func sessionBlockRowDTO(b store.ListSessionBlocksRow) SessionBlock {
	return SessionBlock{
		ID: b.ID, Title: b.Title, DrillID: b.DrillID, DrillName: b.DrillName,
		DurationMin: b.DurationMin, Position: b.Position, Notes: b.Notes,
	}
}

type Session struct {
	ID          uuid.UUID      `json:"id"`
	TeamID      *uuid.UUID     `json:"teamId"`
	Title       string         `json:"title"`
	ScheduledAt *string        `json:"scheduledAt"`
	Notes       *string        `json:"notes"`
	CreatedAt   string         `json:"createdAt"`
	Blocks      []SessionBlock `json:"blocks,omitempty"`
}

func sessionDTO(s store.Session, blocks []SessionBlock) Session {
	var scheduled *string
	if s.ScheduledAt.Valid {
		v := rfc3339(s.ScheduledAt)
		scheduled = &v
	}
	return Session{
		ID: s.ID, TeamID: s.TeamID, Title: s.Title, ScheduledAt: scheduled,
		Notes: s.Notes, CreatedAt: rfc3339(s.CreatedAt), Blocks: blocks,
	}
}

type Game struct {
	ID            uuid.UUID `json:"id"`
	TeamID        uuid.UUID `json:"teamId"`
	Opponent      *string   `json:"opponent"`
	KickoffAt     *string   `json:"kickoffAt"`
	HomeAway      *string   `json:"homeAway"`
	OurScore      *int32    `json:"ourScore"`
	OpponentScore *int32    `json:"opponentScore"`
	Status        string    `json:"status"`
	CreatedAt     string    `json:"createdAt"`
}

func gameDTO(g store.Game) Game {
	var kickoff *string
	if g.KickoffAt.Valid {
		v := rfc3339(g.KickoffAt)
		kickoff = &v
	}
	return Game{
		ID: g.ID, TeamID: g.TeamID, Opponent: g.Opponent, KickoffAt: kickoff,
		HomeAway: g.HomeAway, OurScore: g.OurScore, OpponentScore: g.OpponentScore,
		Status: g.Status, CreatedAt: rfc3339(g.CreatedAt),
	}
}

// AttendanceEntry is one line of the sheet: a person, and both halves of what is known
// about them at this event. Every field of both halves is nullable, because "has not
// replied" and "not recorded yet" are the state most lines are in most of the time.
type AttendanceEntry struct {
	PersonID     uuid.UUID `json:"personId"`
	DisplayName  string    `json:"displayName"`
	JerseyNumber *int32    `json:"jerseyNumber"`
	Position     *string   `json:"position"`
	// OnRoster is false for someone who was at the event but has since left the team.
	// Their line stays on the sheet -- a player who left in March was still at February's
	// match -- and this is what lets a client show it as history rather than as a squad
	// member who has not replied.
	OnRoster   bool       `json:"onRoster"`
	RSVP       *string    `json:"rsvp"`
	RSVPNote   *string    `json:"rsvpNote"`
	RSVPAt     *string    `json:"rsvpAt"`
	RSVPBy     *uuid.UUID `json:"rsvpBy"`
	Status     *string    `json:"status"`
	StatusNote *string    `json:"statusNote"`
	RecordedAt *string    `json:"recordedAt"`
	RecordedBy *uuid.UUID `json:"recordedBy"`
}

func attendanceEntryDTO(r store.ListAttendanceForEventRow) AttendanceEntry {
	return AttendanceEntry{
		PersonID: r.PersonID, DisplayName: r.DisplayName, JerseyNumber: r.JerseyNumber,
		Position: r.Position, OnRoster: r.OnRoster,
		RSVP: r.Rsvp, RSVPNote: r.RsvpNote, RSVPAt: timePtr(r.RsvpAt), RSVPBy: r.RsvpByPersonID,
		Status: r.Status, StatusNote: r.StatusNote, RecordedAt: timePtr(r.RecordedAt),
		RecordedBy: r.RecordedByPersonID,
	}
}

// AttendanceCounts is the squad's tally, which is what a coach reads before they read any
// line. Both halves carry a "neither" bucket: a squad nobody has asked and a squad that
// answered no are the same size and mean opposite things.
type AttendanceCounts struct {
	Going       int `json:"going"`
	Maybe       int `json:"maybe"`
	NotGoing    int `json:"notGoing"`
	NoReply     int `json:"noReply"`
	Present     int `json:"present"`
	Absent      int `json:"absent"`
	Late        int `json:"late"`
	Excused     int `json:"excused"`
	NotRecorded int `json:"notRecorded"`
}

// AttendanceSheet is one event's attendance, whole. EventType names which of the two
// scheduled things this is, so a client holding a sheet knows what to call it without
// remembering which endpoint it asked.
type AttendanceSheet struct {
	EventType string            `json:"eventType"`
	EventID   uuid.UUID         `json:"eventId"`
	TeamID    uuid.UUID         `json:"teamId"`
	Counts    AttendanceCounts  `json:"counts"`
	Entries   []AttendanceEntry `json:"entries"`
}

// AttendanceRecord is one player's season, rather than one player's Saturday: the same
// register counted down instead of across.
type AttendanceRecord struct {
	PersonID     uuid.UUID `json:"personId"`
	DisplayName  string    `json:"displayName"`
	JerseyNumber *int32    `json:"jerseyNumber"`
	Present      int64     `json:"present"`
	Absent       int64     `json:"absent"`
	Late         int64     `json:"late"`
	Excused      int64     `json:"excused"`
	// NotRecorded is the honest half of every number above it. A squad nobody registered
	// and a squad that all turned up look identical without it, and a coach reading a
	// column of zeroes deserves to know which of the two they are looking at.
	NotRecorded int64 `json:"notRecorded"`
	// NoShows said they were coming and did not turn up. No per-fixture sheet can show
	// this: it only exists once the two halves of a line are read together over a season.
	NoShows int64 `json:"noShows"`
	// Rate is present+late over present+late+absent — turned up, of the times we know
	// whether they turned up. Excused is deliberately in neither half: an approved
	// absence is not a player letting the team down, and counting it against them would
	// make the number punish the families who called ahead. Null when nothing was
	// recorded either way, because a rate over no observations is not zero, it is unknown.
	Rate *float64 `json:"rate"`
}

func attendanceRecordDTO(r store.AggregateAttendanceForTeamRow) AttendanceRecord {
	rec := AttendanceRecord{
		PersonID: r.PersonID, DisplayName: r.DisplayName, JerseyNumber: r.JerseyNumber,
		Present: r.Present, Absent: r.Absent, Late: r.Late, Excused: r.Excused,
		NotRecorded: r.NotRecorded, NoShows: r.NoShows,
	}
	if known := r.Present + r.Late + r.Absent; known > 0 {
		rate := float64(r.Present+r.Late) / float64(known)
		rec.Rate = &rate
	}
	return rec
}

// TeamAttendance is the squad's record over a window.
type TeamAttendance struct {
	TeamID uuid.UUID `json:"teamId"`
	// Events is how many fixtures the window covered, which is the denominator every
	// number below is read against. It stays the squad's even when the records are
	// narrowed to one family, for the reason AttendanceSheet's counts do.
	Events  int64              `json:"events"`
	From    *string            `json:"from"`
	To      *string            `json:"to"`
	Records []AttendanceRecord `json:"records"`
}

type ScoreAggregate struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Samples int64   `json:"samples"`
	Average float64 `json:"average"`
	Minimum float64 `json:"minimum"`
	Maximum float64 `json:"maximum"`
}

func aggregateDTO(r store.AggregateScoresForPersonRow) ScoreAggregate {
	return ScoreAggregate{
		Key: r.Key, Label: r.Label, Samples: r.Samples,
		Average: r.Average, Minimum: r.Minimum, Maximum: r.Maximum,
	}
}
