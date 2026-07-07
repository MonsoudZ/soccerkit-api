package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	oc, err := s.resolveOrg(r)
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
	tpl, err := s.store.GetFormTemplate(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("template not found"))
		return
	} else if err != nil {
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
	if !oc.hasAnyRole("admin", "director", "coach") {
		writeError(w, errForbidden("only coaches can create templates"))
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
	for _, f := range req.Fields {
		if f.Key == "" || !validFieldKinds[f.Kind] {
			writeError(w, errValidation("each field needs a key and a valid kind"))
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
	if !oc.hasAnyRole("admin", "director", "coach", "parent", "player") {
		writeError(w, errForbidden("not permitted"))
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

	template, err := s.store.GetFormTemplate(r.Context(), templateID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errBadRequest("templateId does not reference a template"))
		return
	} else if err != nil {
		writeError(w, err)
		return
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
		field, ok := fieldByKey[a.Key]
		if !ok {
			writeError(w, errValidation("unknown field key: "+a.Key))
			return
		}
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
	instance, err := s.store.GetFormInstance(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("instance not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
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
