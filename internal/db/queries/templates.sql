-- name: ListCustomTemplates :many
SELECT * FROM minutes_templates ORDER BY created_at;

-- name: GetCustomTemplate :one
SELECT * FROM minutes_templates WHERE id = $1;

-- name: CreateCustomTemplate :one
INSERT INTO minutes_templates (name, guide, created_by) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateCustomTemplate :one
UPDATE minutes_templates SET name = $2, guide = $3, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteCustomTemplate :execrows
DELETE FROM minutes_templates WHERE id = $1;
