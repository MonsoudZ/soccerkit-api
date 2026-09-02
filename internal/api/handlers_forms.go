package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/authz"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

// seedDefaultTemplates plants the pre/post-game habit-loop templates for a new
// org — the generalization of the app's original PreMatchCheckIn and
// GamePlayerReport structs, now as data rather than code.
func seedDefaultTemplates(ctx context.Context, q *store.Queries, orgID, authorID uuid.UUID) error {
	scaleCfg, _ := json.Marshal(map[string]int{"min": 1, "max": 5})

	type fieldSpec struct {
		key, label, kind string
		cfg              []byte
	}
	seed := func(context, name string, fields []fieldSpec) error {
		tpl, err := q.CreateFormTemplate(ctx, store.CreateFormTemplateParams{
			OrganizationID: &orgID, AuthorPersonID: &authorID, Context: context,
			Name: name, SubjectType: "athlete", IsSeed: ptr(true),
		})
		if err != nil {
			return err
		}
		for i, f := range fields {
			if _, err := q.CreateFormField(ctx, store.CreateFormFieldParams{
				TemplateID: tpl.ID, Key: f.key, Label: f.label, Kind: f.kind,
				Position: int32(i), Config: f.cfg,
			}); err != nil {
				return err
			}
		}
		return nil
	}

	if err := seed("pre_game", "Pre-Game Check-In", []fieldSpec{
		{"sleep", "Sleep quality", "scale", scaleCfg},
		{"nutrition", "Nutrition", "scale", scaleCfg},
		{"hydration", "Hydration", "scale", scaleCfg},
		{"energy", "Energy", "scale", scaleCfg},
		{"focus", "Focus", "scale", scaleCfg},
		{"mood", "Mood", "scale", scaleCfg},
		{"soreness", "Soreness", "scale", scaleCfg},
		{"confidence", "Confidence", "scale", scaleCfg},
		{"warmed_up", "Warmed up", "bool", nil},
		{"has_pain", "Has pain", "bool", nil},
	}); err != nil {
		return err
	}

	return seed("post_game", "Post-Game Report", []fieldSpec{
		{"effort", "Effort", "scale", scaleCfg},
		{"goals", "Goals", "number", nil},
		{"assists", "Assists", "number", nil},
		{"minutes", "Minutes played", "number", nil},
		{"development_focus", "Development focus", "text", nil},
	})
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapTemplateRead, "you cannot see this organization's evaluation templates")
	if err != nil {
		writeError(w, err)
		return
	}
	personID := personIDFrom(r.Context())
	templates, err := s.store.ListFormTemplates(r.Context(), store.ListFormTemplatesParams{
		OrganizationID: &oc.orgID, AuthorPersonID: &personID, Context: queryStrPtr(r, "context"),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]FormTemplate, len(templates))
	for i, t := range templates {
		out[i] = templateDTO(t, nil)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	tpl, err := s.templateFor(r, oc, id)
	if err != nil {
		writeError(w, err)
		return
	}
	fields, err := s.store.ListFormFields(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	fieldDTOs := make([]FormField, len(fields))
	for i, f := range fields {
		fieldDTOs[i] = fieldDTO(f)
	}
	writeJSON(w, http.StatusOK, templateDTO(tpl, fieldDTOs))
}

type createTemplateRequest struct {
	Context     string `json:"context"`
	Name        string `json:"name"`
	SubjectType string `json:"subjectType"`
	Fields      []struct {
		Key    string `json:"key"`
		Label  string `json:"label"`
		Kind   string `json:"kind"`
		Config any    `json:"config"`
	} `json:"fields"`
}

var validContexts = map[string]bool{
	"tryout": true, "pre_game": true, "post_game": true,
	"development": true, "movement": true, "coach_review": true,
}
var validFieldKinds = map[string]bool{
	"scale": true, "bool": true, "number": true, "text": true, "select": true,
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.can(authz.CapTemplateWrite) {
		writeError(w, errForbidden("you cannot create evaluation templates in this organization"))
		return
	}
	var req createTemplateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !validContexts[req.Context] {
		writeError(w, errValidation("invalid context"))
		return
	}
	if req.Name == "" || len(req.Fields) == 0 {
		writeError(w, errValidation("name and at least one field are required"))
		return
	}
	subjectType := req.SubjectType
	if subjectType == "" {
		subjectType = "athlete"
	}
	// form_fields is UNIQUE (template_id, key), so a repeated key used to reach the
	// index and come back as a 500. It is the same mistake handleSubmitInstance already
	// rejects by name on the answer side, and it deserves the same answer here.
	seenKeys := make(map[string]bool, len(req.Fields))
	for _, f := range req.Fields {
		if f.Key == "" || !validFieldKinds[f.Kind] {
			writeError(w, errValidation("each field needs a key and a valid kind"))
			return
		}
		if seenKeys[f.Key] {
			writeError(w, errValidation("duplicate field key: "+f.Key))
			return
		}
		seenKeys[f.Key] = true
	}

	personID := personIDFrom(r.Context())
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	tpl, err := q.CreateFormTemplate(r.Context(), store.CreateFormTemplateParams{
		OrganizationID: &oc.orgID, AuthorPersonID: &personID,
		Context: req.Context, Name: req.Name, SubjectType: subjectType,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	fieldDTOs := make([]FormField, len(req.Fields))
	for i, f := range req.Fields {
		var cfg []byte
		if f.Config != nil {
			cfg, _ = json.Marshal(f.Config)
		}
		created, err := q.CreateFormField(r.Context(), store.CreateFormFieldParams{
			TemplateID: tpl.ID, Key: f.Key, Label: f.Label, Kind: f.Kind,
			Position: int32(i), Config: cfg,
		})
		if err != nil {
			writeError(w, err)
			return
		}
		fieldDTOs[i] = fieldDTO(created)
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, templateDTO(tpl, fieldDTOs))
}

type submitInstanceRequest struct {
	TemplateID      string  `json:"templateId"`
	SubjectPersonID *string `json:"subjectPersonId"`
	SubjectTeamID   *string `json:"subjectTeamId"`
	ContextRefType  *string `json:"contextRefType"`
	ContextRefID    *string `json:"contextRefId"`
	Answers         []struct {
		Key          string   `json:"key"`
		NumericValue *float64 `json:"numericValue"`
		BoolValue    *bool    `json:"boolValue"`
		TextValue    *string  `json:"textValue"`
	} `json:"answers"`
}

// handleSubmitInstance records one filled-out evaluation: the instance plus its
// normalized answers, in a transaction.
func (s *Server) handleSubmitInstance(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.can(authz.CapEvaluationSubmit) {
		writeError(w, errForbidden("you cannot submit evaluations in this organization"))
		return
	}
	var req submitInstanceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	templateID, err := parseUUIDParam(req.TemplateID, "templateId")
	if err != nil {
		writeError(w, err)
		return
	}
	if req.SubjectPersonID == nil && req.SubjectTeamID == nil {
		writeError(w, errValidation("a subjectPersonId or subjectTeamId is required"))
		return
	}
	var subjectPerson, subjectTeam, ctxRefID *uuid.UUID
	if req.SubjectPersonID != nil {
		id, perr := parseUUIDParam(*req.SubjectPersonID, "subjectPersonId")
		if perr != nil {
			writeError(w, perr)
			return
		}
		subjectPerson = &id
	}
	if req.SubjectTeamID != nil {
		id, perr := parseUUIDParam(*req.SubjectTeamID, "subjectTeamId")
		if perr != nil {
			writeError(w, perr)
			return
		}
		subjectTeam = &id
	}
	if req.ContextRefID != nil {
		id, perr := parseUUIDParam(*req.ContextRefID, "contextRefId")
		if perr != nil {
			writeError(w, perr)
			return
		}
		ctxRefID = &id
	}

	template, err := s.templateFor(r, oc, templateID)
	if err != nil {
		writeError(w, err)
		return
	}
	// The subject must be someone this organization can evaluate. Without this, an
	// instance could be filed against another club's athlete, and the scores would land
	// in that athlete's aggregate.
	if subjectPerson != nil {
		if err := s.personVisibleTo(r.Context(), oc, *subjectPerson); err != nil {
			writeError(w, err)
			return
		}
	}
	if subjectTeam != nil {
		if _, err := s.teamByIDInOrg(r.Context(), oc, *subjectTeam); err != nil {
			writeError(w, err)
			return
		}
	}
	fields, err := s.store.ListFormFields(r.Context(), templateID)
	if err != nil {
		writeError(w, err)
		return
	}
	fieldByKey := make(map[string]store.FormField, len(fields))
	for _, f := range fields {
		fieldByKey[f.Key] = f
	}

	// Validate every answer before opening the transaction. The aggregate endpoint
	// averages numeric_value across instances, so an unvalidated answer is not a bad
	// row in isolation — it permanently skews that athlete's trend.
	seen := make(map[string]bool, len(req.Answers))
	for _, a := range req.Answers {
		field, ok := fieldByKey[a.Key]
		if !ok {
			writeError(w, errValidation("unknown field key: "+a.Key))
			return
		}
		// CreateFormAnswer upserts on (instance_id, field_id), so a repeated key used to
		// silently overwrite the earlier answer — usually with NULL — while the response
		// echoed both as though both had been stored.
		if seen[a.Key] {
			writeError(w, errValidation("duplicate answer for field: "+a.Key))
			return
		}
		seen[a.Key] = true
		if err := validateAnswer(field, a.NumericValue, a.BoolValue, a.TextValue); err != nil {
			writeError(w, err)
			return
		}
	}

	personID := personIDFrom(r.Context())
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	instance, err := q.CreateFormInstance(r.Context(), store.CreateFormInstanceParams{
		TemplateID: template.ID, SubjectPersonID: subjectPerson, SubjectTeamID: subjectTeam,
		ContextRefType: req.ContextRefType, ContextRefID: ctxRefID, SubmittedByPersonID: &personID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	answerDTOs := make([]Answer, 0, len(req.Answers))
	for _, a := range req.Answers {
		field := fieldByKey[a.Key]
		if _, err := q.CreateFormAnswer(r.Context(), store.CreateFormAnswerParams{
			InstanceID: instance.ID, FieldID: field.ID,
			NumericValue: a.NumericValue, BoolValue: a.BoolValue, TextValue: a.TextValue,
		}); err != nil {
			writeError(w, err)
			return
		}
		answerDTOs = append(answerDTOs, Answer{
			Key: field.Key, Label: field.Label, Kind: field.Kind,
			NumericValue: a.NumericValue, BoolValue: a.BoolValue, TextValue: a.TextValue,
		})
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, FormInstance{
		ID: instance.ID, TemplateID: instance.TemplateID, Context: template.Context,
		SubjectPersonID: instance.SubjectPersonID, SubjectTeamID: instance.SubjectTeamID,
		ContextRefType: instance.ContextRefType, ContextRefID: instance.ContextRefID,
		SubmittedAt: rfc3339(instance.SubmittedAt), Answers: answerDTOs,
	})
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	oc, err := s.requireCapability(r, authz.CapEvaluationRead, "you cannot read evaluations in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	instance, err := s.store.GetFormInstance(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("instance not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	// An instance is as sensitive as its subject: it carries that athlete's scored
	// answers. Gate on the subject rather than the template, because a template can be
	// shared while the answers about a given child are not.
	if instance.SubjectPersonID != nil {
		if err := s.personVisibleTo(r.Context(), oc, *instance.SubjectPersonID); err != nil {
			writeError(w, errNotFound("instance not found"))
			return
		}
	}
	if instance.SubjectTeamID != nil {
		if _, err := s.teamByIDInOrg(r.Context(), oc, *instance.SubjectTeamID); err != nil {
			writeError(w, errNotFound("instance not found"))
			return
		}
	}
	template, err := s.store.GetFormTemplate(r.Context(), instance.TemplateID)
	if err != nil {
		writeError(w, err)
		return
	}
	answers, err := s.store.ListAnswersForInstance(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	answerDTOs := make([]Answer, len(answers))
	for i, a := range answers {
		answerDTOs[i] = answerRowDTO(a)
	}
	writeJSON(w, http.StatusOK, FormInstance{
		ID: instance.ID, TemplateID: instance.TemplateID, Context: template.Context,
		SubjectPersonID: instance.SubjectPersonID, SubjectTeamID: instance.SubjectTeamID,
		ContextRefType: instance.ContextRefType, ContextRefID: instance.ContextRefID,
		SubmittedAt: rfc3339(instance.SubmittedAt), Answers: answerDTOs,
	})
}

// templateFor loads a form template and verifies the caller may use it: it belongs to
// their organization, or they authored it (organization_id is NULL for the personal
// templates a coach carries between clubs). Both columns are nullable, so both are
// nil-checked before comparing.
func (s *Server) templateFor(r *http.Request, oc orgContext, id uuid.UUID) (store.FormTemplate, error) {
	tpl, err := s.store.GetFormTemplate(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.FormTemplate{}, errNotFound("template not found")
	} else if err != nil {
		return store.FormTemplate{}, err
	}
	callerID := personIDFrom(r.Context())
	inOrg := tpl.OrganizationID != nil && *tpl.OrganizationID == oc.orgID
	mine := tpl.AuthorPersonID != nil && *tpl.AuthorPersonID == callerID
	if !inOrg && !mine {
		return store.FormTemplate{}, errNotFound("template not found")
	}
	return tpl, nil
}

// maxAnswerMagnitude bounds every numeric answer. A "scale" field declares its own
// range in config and a "number" field declares nothing, so 1e308 was a valid number of
// goals — and two of them overflowed the running sum inside
// AggregateScoresForPerson's avg(), which failed the whole query. Not just that key:
// the aggregate is one GROUP BY over every answer about the athlete, so an ordinary
// check-in filed afterwards was unreadable too, and no endpoint deletes a form instance
// or an answer, so it stayed that way.
//
// 1e12 is far outside anything a coach records (goals, minutes, a 1-5 scale) and far
// inside float8's ceiling, which is the property that matters: the sum of any realistic
// number of these cannot overflow. The query was also made overflow-proof, because
// validation added today does nothing about rows written yesterday.
const maxAnswerMagnitude = 1e12

// scaleConfig is the shape a "scale" field stores in form_fields.config.
type scaleConfig struct {
	Min *float64 `json:"min"`
	Max *float64 `json:"max"`
}

// validateAnswer checks one answer against the field it claims to answer. Each kind
// requires its own value column and rejects the others, so a bool field can no longer
// be handed a number and end up in the score aggregate.
func validateAnswer(field store.FormField, numeric *float64, boolean *bool, text *string) error {
	provided := 0
	for _, present := range []bool{numeric != nil, boolean != nil, text != nil} {
		if present {
			provided++
		}
	}
	if provided == 0 {
		return errValidation("answer for " + field.Key + " has no value")
	}
	if provided > 1 {
		return errValidation("answer for " + field.Key + " must set exactly one of numericValue, boolValue or textValue")
	}

	switch field.Kind {
	case "scale", "number":
		if numeric == nil {
			return errValidation(field.Key + " is a " + field.Kind + " field and needs a numericValue")
		}
		if math.IsNaN(*numeric) || math.IsInf(*numeric, 0) {
			return errValidation(field.Key + " must be a finite number")
		}
		if math.Abs(*numeric) > maxAnswerMagnitude {
			return errValidation(fmt.Sprintf("%s must be between %g and %g",
				field.Key, -maxAnswerMagnitude, maxAnswerMagnitude))
		}
		if field.Kind == "scale" {
			return validateScaleRange(field, *numeric)
		}
	case "bool":
		if boolean == nil {
			return errValidation(field.Key + " is a bool field and needs a boolValue")
		}
	case "text", "select":
		if text == nil {
			return errValidation(field.Key + " is a " + field.Kind + " field and needs a textValue")
		}
	}
	return nil
}

// validateScaleRange enforces the min/max a scale field declares in its config. A
// template author is free to omit either bound, in which case that end is unbounded.
func validateScaleRange(field store.FormField, v float64) error {
	if len(field.Config) == 0 {
		return nil
	}
	var cfg scaleConfig
	if err := json.Unmarshal(field.Config, &cfg); err != nil {
		return nil // a config we cannot read is not the submitter's problem
	}
	if cfg.Min != nil && v < *cfg.Min {
		return errValidation(fmt.Sprintf("%s must be at least %g", field.Key, *cfg.Min))
	}
	if cfg.Max != nil && v > *cfg.Max {
		return errValidation(fmt.Sprintf("%s must be at most %g", field.Key, *cfg.Max))
	}
	return nil
}
