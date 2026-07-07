-- name: CreateVenue :one
INSERT INTO venues (name, address, city, latitude, longitude, surface)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetVenue :one
SELECT * FROM venues WHERE id = $1;

-- name: ListVenues :many
SELECT * FROM venues
WHERE (sqlc.narg('city')::text IS NULL OR city ILIKE '%' || sqlc.narg('city') || '%')
ORDER BY name ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');
