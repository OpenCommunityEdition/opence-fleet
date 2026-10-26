-- name: CreateJob :one
INSERT INTO jobs (kind, title, params, state, actor, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = ?;

-- name: ListJobs :many
SELECT * FROM jobs ORDER BY id DESC LIMIT ?;

-- name: ListUnfinishedJobs :many
SELECT * FROM jobs WHERE state IN ('queued', 'running') ORDER BY id;

-- name: UpdateJob :exec
UPDATE jobs SET state = ?, finished_at = ?, detail = ? WHERE id = ?;

-- name: CreateJobItem :exec
INSERT INTO job_items (job_id, slug, wave, state, detail, updated_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: UpdateJobItem :exec
UPDATE job_items SET state = ?, detail = ?, updated_at = ? WHERE job_id = ? AND slug = ?;

-- name: ListJobItems :many
SELECT * FROM job_items WHERE job_id = ? ORDER BY wave, slug;

-- name: CreateSchedule :one
INSERT INTO schedules (name, cron, action, targets, params, enabled, next_run) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetSchedule :one
SELECT * FROM schedules WHERE id = ?;

-- name: ListSchedules :many
SELECT * FROM schedules ORDER BY name;

-- name: UpdateSchedule :exec
UPDATE schedules SET name = ?, cron = ?, action = ?, targets = ?, params = ?, enabled = ?, next_run = ? WHERE id = ?;

-- name: UpdateScheduleRun :exec
UPDATE schedules SET last_run = ?, next_run = ?, last_job = ? WHERE id = ?;

-- name: DeleteSchedule :exec
DELETE FROM schedules WHERE id = ?;

-- name: CreateWebhook :one
INSERT INTO webhooks (label, url_enc, kind, rules, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE id = ?;

-- name: ListWebhooks :many
SELECT * FROM webhooks ORDER BY label;

-- name: UpdateWebhook :exec
UPDATE webhooks SET label = ?, kind = ?, rules = ? WHERE id = ?;

-- name: UpdateWebhookStatus :exec
UPDATE webhooks SET last_status = ? WHERE id = ?;

-- name: DeleteWebhook :exec
DELETE FROM webhooks WHERE id = ?;

-- name: OpenAlert :one
INSERT INTO alerts (rule, target, detail, first_seen, last_seen) VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: GetOpenAlert :one
SELECT * FROM alerts WHERE rule = ? AND target = ? AND resolved_at IS NULL LIMIT 1;

-- name: GetAlert :one
SELECT * FROM alerts WHERE id = ?;

-- name: TouchAlert :exec
UPDATE alerts SET last_seen = ?, detail = ? WHERE id = ?;

-- name: ResolveAlert :exec
UPDATE alerts SET resolved_at = ? WHERE id = ?;

-- name: AcknowledgeAlert :exec
UPDATE alerts SET acknowledged_by = ? WHERE id = ?;

-- name: SetAlertDelivered :exec
UPDATE alerts SET delivered = ? WHERE id = ?;

-- name: ListOpenAlerts :many
SELECT * FROM alerts WHERE resolved_at IS NULL ORDER BY first_seen DESC;

-- name: ListAlerts :many
SELECT * FROM alerts ORDER BY id DESC LIMIT ?;

-- name: InsertAudit :exec
INSERT INTO audit (time, actor, action, target, detail) VALUES (?, ?, ?, ?, ?);

-- name: ListAudit :many
SELECT * FROM audit
WHERE (CAST(sqlc.arg(actor) AS TEXT) = '' OR actor LIKE CAST(sqlc.arg(actor) AS TEXT) || '%')
  AND (CAST(sqlc.arg(action) AS TEXT) = '' OR action LIKE CAST(sqlc.arg(action) AS TEXT) || '%')
  AND (CAST(sqlc.arg(target) AS TEXT) = '' OR target LIKE '%' || CAST(sqlc.arg(target) AS TEXT) || '%')
  AND (CAST(sqlc.arg(since) AS INTEGER) = 0 OR time >= CAST(sqlc.arg(since) AS INTEGER))
  AND (CAST(sqlc.arg(before) AS INTEGER) = 0 OR id < CAST(sqlc.arg(before) AS INTEGER))
ORDER BY id DESC LIMIT sqlc.arg(lim);

-- name: GetSetting :one
SELECT value FROM settings WHERE name = ?;

-- name: SetSetting :exec
INSERT INTO settings (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value;

-- name: ListSettings :many
SELECT * FROM settings ORDER BY name;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE name = ?;
