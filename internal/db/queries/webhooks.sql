-- name: ListWebhooks :many
SELECT * FROM webhooks ORDER BY created_at, id;

-- name: CountWebhooks :one
SELECT count(*) FROM webhooks;

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE id = $1;

-- name: CreateWebhook :one
INSERT INTO webhooks (platform, name, url, url_masked, secret, auto_push, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateWebhook :one
UPDATE webhooks
SET platform = $2, name = $3, url = $4, url_masked = $5, secret = $6, auto_push = $7, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks WHERE id = $1;

-- name: AutoPushWebhooks :many
-- 纪要第一次生成后要自动发到的群机器人
SELECT id, name, platform FROM webhooks WHERE auto_push ORDER BY created_at, id;

-- name: CreateWebhookSend :one
INSERT INTO webhook_sends (meeting_id, webhook_id, webhook_name, platform, minutes_version, requested_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetWebhookSend :one
SELECT * FROM webhook_sends WHERE id = $1;

-- name: LatestWebhookSends :many
-- 一场会议发到每个群机器人的最近一次（群机器人已经删掉的不列），带上发送人的名字
SELECT DISTINCT ON (s.webhook_id) sqlc.embed(s), coalesce(u.name, '')::text AS requested_by_name
FROM webhook_sends s LEFT JOIN users u ON u.id = s.requested_by
WHERE s.meeting_id = $1 AND s.webhook_id IS NOT NULL
ORDER BY s.webhook_id, s.created_at DESC;

-- name: PendingWebhookSends :many
-- 这场会议里还没发完的群机器人。发送中超过 window_secs 秒的不算（重试早该结束了，多半是后台任务丢了），免得一直卡住不让发。
-- window_secs 是 service.WebhookPendingWindow，网页上显示的状态用的也是它
SELECT DISTINCT webhook_id FROM webhook_sends
WHERE meeting_id = sqlc.arg(meeting_id) AND status = 'pending' AND webhook_id IS NOT NULL
  AND created_at > now() - make_interval(secs => sqlc.arg(window_secs)::float8);

-- name: FinishWebhookSend :exec
UPDATE webhook_sends SET status = 'done', error = '', finished_at = now() WHERE id = $1;

-- name: NoteWebhookSendRetry :exec
-- 这一次没发出去、稍后自动重试：先把原因写上，网页上能看到为什么还在发送中
UPDATE webhook_sends SET error = $2 WHERE id = $1 AND status = 'pending';

-- name: FailWebhookSend :exec
UPDATE webhook_sends SET status = 'failed', error = $2, finished_at = now() WHERE id = $1;
