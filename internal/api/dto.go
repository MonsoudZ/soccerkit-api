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
