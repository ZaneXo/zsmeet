-- name: CreateShare :one
-- created_at 用写入时的时间（clock_timestamp），不用事务开始的时间（now）：网页上发送是锁住会议以后才写，
-- 等锁的那一段不算进去，记录才按真正的先后排、「发送中」的期限才从真正开始发算起（和 CreateWebhookSend 一样）
INSERT INTO feishu_shares (meeting_id, chat_id, chat_name, with_doc, minutes_version, requested_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp())
RETURNING *;

-- name: GetShare :one
SELECT * FROM feishu_shares WHERE id = $1;

-- name: ListShares :many
-- 一场会议最近发到飞书的记录，最新的在前
SELECT sqlc.embed(s), coalesce(u.name, '')::text AS requested_by_name
FROM feishu_shares s LEFT JOIN users u ON u.id = s.requested_by
WHERE s.meeting_id = $1
ORDER BY s.created_at DESC
LIMIT 20;

-- name: PendingShare :one
-- 这场会议有没有还没发完的。半小时还没发完的不算（重试早该结束了，多半是后台任务丢了），免得一直卡住不让发
SELECT count(*) FROM feishu_shares
WHERE meeting_id = $1 AND status = 'pending' AND created_at > clock_timestamp() - interval '30 minutes';

-- name: SetShareTicket :exec
UPDATE feishu_shares SET import_ticket = $2 WHERE id = $1;

-- name: SetShareDoc :exec
UPDATE feishu_shares SET doc_token = $2, doc_url = $3 WHERE id = $1;

-- name: FinishShare :exec
UPDATE feishu_shares SET status = 'done', error = '', message_id = $2, finished_at = now() WHERE id = $1;

-- name: NoteShareRetry :exec
-- 这一次没发出去、稍后自动重试：先把原因写上，网页上能看到为什么还在发送中
UPDATE feishu_shares SET error = $2 WHERE id = $1 AND status = 'pending';

-- name: FailShare :exec
UPDATE feishu_shares SET status = 'failed', error = $2, finished_at = now() WHERE id = $1;

-- name: SharedBefore :one
-- 这场会议以前有没有把更早的一版纪要发到过这个群（这次发的卡片标成「纪要已更新」）
SELECT count(*) > 0 FROM feishu_shares
WHERE meeting_id = $1 AND chat_id = $2 AND status = 'done' AND minutes_version < sqlc.arg('version');

-- name: DocForVersion :one
-- 这一版纪要已经导入过的飞书文档，发卡片时带上它，再导入时直接用它
SELECT doc_token, doc_url FROM feishu_shares
WHERE meeting_id = $1 AND minutes_version = $2 AND doc_token <> ''
ORDER BY created_at DESC
LIMIT 1;

-- name: GetFeishuImport :one
SELECT * FROM feishu_imports WHERE feishu_meeting_id = $1;

-- name: SaveFeishuImport :exec
-- 记下（或者更新）一场飞书会议的导入进度。已经导入了的不再改
INSERT INTO feishu_imports (feishu_meeting_id, topic, status, detail, owner_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (feishu_meeting_id) DO UPDATE
SET topic = EXCLUDED.topic, status = EXCLUDED.status, detail = EXCLUDED.detail, owner_id = EXCLUDED.owner_id, updated_at = now()
WHERE feishu_imports.status <> 'imported';

-- name: MarkFeishuImported :exec
UPDATE feishu_imports SET status = 'imported', detail = '', meeting_id = $2, updated_at = now()
WHERE feishu_meeting_id = $1;

-- name: RecentFeishuImports :many
-- 最近的飞书会议导入情况。会议已经彻底删除了的不列（主题已经清掉了）
SELECT sqlc.embed(i), u.name AS owner_name
FROM feishu_imports i JOIN users u ON u.id = i.owner_id
WHERE NOT (i.status = 'imported' AND i.meeting_id IS NULL)
ORDER BY i.created_at DESC
LIMIT 10;

-- name: CreateImportedMeeting :one
-- 从飞书会议导入的会议：直接排队处理，时间用飞书会议开始的时间
INSERT INTO meetings (owner_id, title, source, status, run_id, template, created_at)
VALUES ($1, $2, 'feishu', 'queued', $3, 'general', $4)
RETURNING *;

-- name: SetFeishuLink :exec
INSERT INTO feishu_link (id, state, detail, updated_at) VALUES (true, $1, $2, now())
ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state, detail = EXCLUDED.detail, updated_at = now();

-- name: NoteFeishuEvent :exec
INSERT INTO feishu_link (id, last_event_at, last_event) VALUES (true, now(), $1)
ON CONFLICT (id) DO UPDATE SET last_event_at = now(), last_event = EXCLUDED.last_event;

-- name: TouchFeishuLink :exec
-- 长连接的状态没变：只更新时间，表示后台处理（worker）还在运行
UPDATE feishu_link SET updated_at = now() WHERE id;

-- name: GetFeishuLink :one
SELECT * FROM feishu_link WHERE id;
