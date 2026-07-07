-- Templates ----------------------------------------------------------------

-- name: CreateFormTemplate :one
INSERT INTO form_templates (organization_id, author_person_id, context, name, subject_type, version, is_seed)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.narg('version'), 1), COALESCE(sqlc.narg('is_seed'), false))
RETURNING *;

-- name: GetFormTemplate :one
SELECT * FROM form_templates WHERE id = $1;

-- name: ListFormTemplates :many
SELECT * FROM form_templates
WHERE (organization_id = sqlc.narg('organization_id') OR author_person_id = sqlc.narg('author_person_id'))
  AND (sqlc.narg('context')::text IS NULL OR context = sqlc.narg('context'))
ORDER BY context, name;

-- name: CreateFormField :one
INSERT INTO form_fields (template_id, key, label, kind, position, config)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListFormFields :many
SELECT * FROM form_fields WHERE template_id = $1 ORDER BY position, key;

-- name: GetFormFieldByKey :one
SELECT * FROM form_fields WHERE template_id = $1 AND key = $2;

-- Instances & answers -------------------------------------------------------

-- name: CreateFormInstance :one
INSERT INTO form_instances
    (template_id, subject_person_id, subject_team_id, context_ref_type, context_ref_id, submitted_by_person_id, extra)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetFormInstance :one
SELECT * FROM form_instances WHERE id = $1;

-- name: CreateFormAnswer :one
INSERT INTO form_answers (instance_id, field_id, numeric_value, bool_value, text_value)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (instance_id, field_id)
DO UPDATE SET numeric_value = EXCLUDED.numeric_value, bool_value = EXCLUDED.bool_value, text_value = EXCLUDED.text_value
RETURNING *;

-- name: ListAnswersForInstance :many
SELECT fa.*, ff.key, ff.label, ff.kind
FROM form_answers fa
JOIN form_fields ff ON ff.id = fa.field_id
WHERE fa.instance_id = $1
ORDER BY ff.position, ff.key;

-- name: ListInstancesForPerson :many
SELECT fi.id, fi.template_id, ft.context, ft.name AS template_name,
    fi.context_ref_type, fi.context_ref_id, fi.submitted_by_person_id, fi.submitted_at
FROM form_instances fi
JOIN form_templates ft ON ft.id = fi.template_id
WHERE fi.subject_person_id = $1
  AND (sqlc.narg('context')::text IS NULL OR ft.context = sqlc.narg('context'))
ORDER BY fi.submitted_at DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- The moat query: cross-instance aggregation of scored fields for one athlete,
-- optionally scoped to a context. Powers readiness means, effort trends, etc.
-- name: AggregateScoresForPerson :many
SELECT ff.key, ff.label,
    count(fa.numeric_value)::bigint      AS samples,
    avg(fa.numeric_value)::double precision AS average,
    min(fa.numeric_value)::double precision AS minimum,
    max(fa.numeric_value)::double precision AS maximum
FROM form_instances fi
JOIN form_templates ft ON ft.id = fi.template_id
JOIN form_answers fa ON fa.instance_id = fi.id
JOIN form_fields ff ON ff.id = fa.field_id
WHERE fi.subject_person_id = $1
  AND (sqlc.narg('context')::text IS NULL OR ft.context = sqlc.narg('context'))
  AND fa.numeric_value IS NOT NULL
GROUP BY ff.key, ff.label
ORDER BY ff.key;
