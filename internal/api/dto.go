package api

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// rfc3339 formats a non-null timestamp; zero value for invalid.
func rfc3339(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format(time.RFC3339)
}

// ---- users ---------------------------------------------------------------

type PublicUser struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Position    *string   `json:"position"`
	SkillLevel  int32     `json:"skillLevel"`
	Bio         *string   `json:"bio"`
	AvatarURL   *string   `json:"avatarUrl"`
	CreatedAt   string    `json:"createdAt"`
}

func publicUser(u store.User) PublicUser {
	return PublicUser{
		ID: u.ID, Email: u.Email, DisplayName: u.DisplayName,
		Position: u.Position, SkillLevel: u.SkillLevel, Bio: u.Bio,
		AvatarURL: u.AvatarUrl, CreatedAt: rfc3339(u.CreatedAt),
	}
}

type AuthResponse struct {
	AccessToken  string     `json:"accessToken"`
	RefreshToken string     `json:"refreshToken"`
	User         PublicUser `json:"user"`
}

type CareerStats struct {
	Appearances   int64 `json:"appearances"`
	Goals         int64 `json:"goals"`
	Assists       int64 `json:"assists"`
	YellowCards   int64 `json:"yellowCards"`
	RedCards      int64 `json:"redCards"`
	MinutesPlayed int64 `json:"minutesPlayed"`
}

// ---- venues --------------------------------------------------------------

type Venue struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Address   *string   `json:"address"`
	City      *string   `json:"city"`
	Latitude  *float64  `json:"latitude"`
	Longitude *float64  `json:"longitude"`
	Surface   *string   `json:"surface"`
	CreatedAt string    `json:"createdAt"`
}

func venueDTO(v store.Venue) Venue {
	return Venue{
		ID: v.ID, Name: v.Name, Address: v.Address, City: v.City,
		Latitude: v.Latitude, Longitude: v.Longitude, Surface: v.Surface,
		CreatedAt: rfc3339(v.CreatedAt),
	}
}

// ---- matches -------------------------------------------------------------

type Match struct {
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	Description *string    `json:"description"`
	Format      string     `json:"format"`
	MaxPlayers  int32      `json:"maxPlayers"`
	KickoffAt   string     `json:"kickoffAt"`
	DurationMin int32      `json:"durationMin"`
	Status      string     `json:"status"`
	HostID      uuid.UUID  `json:"hostId"`
	VenueID     *uuid.UUID `json:"venueId"`
	GoingCount  int64      `json:"goingCount"`
	SpotsLeft   int64      `json:"spotsLeft"`
	CreatedAt   string     `json:"createdAt"`
}

func matchDTO(m store.Match, goingCount int64) Match {
	spots := int64(m.MaxPlayers) - goingCount
	if spots < 0 {
		spots = 0
	}
	return Match{
		ID: m.ID, Title: m.Title, Description: m.Description, Format: m.Format,
		MaxPlayers: m.MaxPlayers, KickoffAt: rfc3339(m.KickoffAt), DurationMin: m.DurationMin,
		Status: m.Status, HostID: m.HostID, VenueID: m.VenueID,
		GoingCount: goingCount, SpotsLeft: spots, CreatedAt: rfc3339(m.CreatedAt),
	}
}

func matchRowDTO(m store.ListMatchesRow) Match {
	return matchDTO(store.Match{
		ID: m.ID, HostID: m.HostID, VenueID: m.VenueID, Title: m.Title,
		Description: m.Description, Format: m.Format, MaxPlayers: m.MaxPlayers,
		KickoffAt: m.KickoffAt, DurationMin: m.DurationMin, Status: m.Status,
		CreatedAt: m.CreatedAt,
	}, m.GoingCount)
}

type Rsvp struct {
	ID        uuid.UUID  `json:"id"`
	Status    string     `json:"status"`
	CreatedAt string     `json:"createdAt"`
	User      PublicUser `json:"user"`
}

func rsvpRowDTO(r store.ListMatchRsvpsRow) Rsvp {
	return Rsvp{
		ID: r.ID, Status: r.Status, CreatedAt: rfc3339(r.CreatedAt),
		User: PublicUser{
			ID: r.UserID, Email: r.Email, DisplayName: r.DisplayName,
			Position: r.Position, SkillLevel: r.SkillLevel, Bio: r.Bio,
			AvatarURL: r.AvatarUrl, CreatedAt: rfc3339(r.UserCreatedAt),
		},
	}
}

type MatchDetail struct {
	Match
	Rsvps []Rsvp `json:"rsvps"`
}

// ---- teams ---------------------------------------------------------------

type Team struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	CrestURL    *string   `json:"crestUrl"`
	OwnerID     uuid.UUID `json:"ownerId"`
	MemberCount int64     `json:"memberCount"`
	CreatedAt   string    `json:"createdAt"`
}

func teamDTO(t store.Team, memberCount int64) Team {
	return Team{
		ID: t.ID, Name: t.Name, CrestURL: t.CrestUrl, OwnerID: t.OwnerID,
		MemberCount: memberCount, CreatedAt: rfc3339(t.CreatedAt),
	}
}

type TeamMember struct {
	ID           uuid.UUID  `json:"id"`
	Role         string     `json:"role"`
	JerseyNumber *int32     `json:"jerseyNumber"`
	JoinedAt     string     `json:"joinedAt"`
	User         PublicUser `json:"user"`
}

func teamMemberRowDTO(m store.ListTeamMembersRow) TeamMember {
	return TeamMember{
		ID: m.ID, Role: m.Role, JerseyNumber: m.JerseyNumber, JoinedAt: rfc3339(m.JoinedAt),
		User: PublicUser{
			ID: m.UserID, Email: m.Email, DisplayName: m.DisplayName,
			Position: m.Position, SkillLevel: m.SkillLevel, Bio: m.Bio,
			AvatarURL: m.AvatarUrl, CreatedAt: rfc3339(m.UserCreatedAt),
		},
	}
}

type TeamDetail struct {
	Team
	Members []TeamMember `json:"members"`
}

// ---- leagues -------------------------------------------------------------

type League struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Season    string    `json:"season"`
	TeamCount int64     `json:"teamCount"`
	CreatedAt string    `json:"createdAt"`
}

type Fixture struct {
	ID           uuid.UUID `json:"id"`
	LeagueID     uuid.UUID `json:"leagueId"`
	HomeTeamID   uuid.UUID `json:"homeTeamId"`
	AwayTeamID   uuid.UUID `json:"awayTeamId"`
	HomeTeamName string    `json:"homeTeamName"`
	AwayTeamName string    `json:"awayTeamName"`
	KickoffAt    string    `json:"kickoffAt"`
	HomeScore    *int32    `json:"homeScore"`
	AwayScore    *int32    `json:"awayScore"`
	Status       string    `json:"status"`
}

type StandingRow struct {
	TeamID         uuid.UUID `json:"teamId"`
	TeamName       string    `json:"teamName"`
	Played         int       `json:"played"`
	Won            int       `json:"won"`
	Drawn          int       `json:"drawn"`
	Lost           int       `json:"lost"`
	GoalsFor       int       `json:"goalsFor"`
	GoalsAgainst   int       `json:"goalsAgainst"`
	GoalDifference int       `json:"goalDifference"`
	Points         int       `json:"points"`
}

// ---- stats ---------------------------------------------------------------

type PlayerStat struct {
	ID            uuid.UUID   `json:"id"`
	UserID        uuid.UUID   `json:"userId"`
	MatchID       *uuid.UUID  `json:"matchId"`
	FixtureID     *uuid.UUID  `json:"fixtureId"`
	Goals         int32       `json:"goals"`
	Assists       int32       `json:"assists"`
	YellowCards   int32       `json:"yellowCards"`
	RedCards      int32       `json:"redCards"`
	MinutesPlayed int32       `json:"minutesPlayed"`
	User          *PublicUser `json:"user,omitempty"`
}

func playerStatDTO(s store.PlayerMatchStat, user *PublicUser) PlayerStat {
	return PlayerStat{
		ID: s.ID, UserID: s.UserID, MatchID: s.MatchID, FixtureID: s.FixtureID,
		Goals: s.Goals, Assists: s.Assists, YellowCards: s.YellowCards,
		RedCards: s.RedCards, MinutesPlayed: s.MinutesPlayed, User: user,
	}
}
