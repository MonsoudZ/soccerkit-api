package api

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/monsoudz/soccerkit-api/internal/authz"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

func rfc3339(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format(time.RFC3339)
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

// ---- roles & access ------------------------------------------------------

// RoleInfo is one row of the published role catalogue (GET /roles): what the role is
// called, what it means, and the exact capabilities it carries. Capabilities are sent
// as strings rather than a fixed set of booleans so a new one appears in the client's
// hands the day it is defined, without a wire change.
type RoleInfo struct {
	Role         string   `json:"role"`
	Label        string   `json:"label"`
	Description  string   `json:"description"`
	Rank         int      `json:"rank"`
	Capabilities []string `json:"capabilities"`
}

func roleInfoDTO(r authz.Role) RoleInfo {
	return RoleInfo{
		Role: string(r), Label: r.Label(), Description: r.Description(), Rank: r.Rank(),
		Capabilities: capabilityNames(r.Capabilities()),
	}
}

// Access is what the caller may do in the organization they are acting in — the union
// of their roles there, not one role. `scope` says how far their reads reach: "org" for
// staff, "own" for a parent or player, who see their own household only.
type Access struct {
	OrganizationID uuid.UUID `json:"organizationId"`
	Roles          []string  `json:"roles"`
	Capabilities   []string  `json:"capabilities"`
	Scope          string    `json:"scope"`
	GrantableRoles []string  `json:"grantableRoles"`
}

func accessDTO(oc orgContext) Access {
	return Access{
		OrganizationID: oc.orgID,
		Roles:          roleNames(oc.roles.Roles()),
		Capabilities:   capabilityNames(oc.roles.Capabilities()),
		Scope:          string(oc.scope()),
		GrantableRoles: roleNames(oc.roles.GrantableRoles()),
	}
}

// OrgMember is one person in an organization with every role they hold there. The solo
// coach holds three and is one member, not three rows.
type OrgMember struct {
	PersonID    uuid.UUID `json:"personId"`
	DisplayName string    `json:"displayName"`
	Email       *string   `json:"email"`
	Roles       []string  `json:"roles"`
	JoinedAt    string    `json:"joinedAt"`
}

func memberDTO(m store.ListOrgMembersRow) OrgMember {
	return OrgMember{
		PersonID: m.PersonID, DisplayName: m.DisplayName, Email: m.Email,
		Roles:    roleNames(authz.NewSet(m.Roles...).Roles()),
		JoinedAt: rfc3339(m.JoinedAt),
	}
}

// MemberRoles is what a grant or revoke answers with: the person's roles as they stand
// after the write.
type MemberRoles struct {
	PersonID       uuid.UUID `json:"personId"`
	OrganizationID uuid.UUID `json:"organizationId"`
	Roles          []string  `json:"roles"`
}

// roleNames and capabilityNames render the typed values onto the wire. Both always
// produce an array, never null: a client that has to distinguish "no roles" from
// "missing" has been handed a bug rather than a value.
func roleNames(roles []authz.Role) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

func capabilityNames(caps []authz.Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
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
