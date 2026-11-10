-- name: CreateMeeting :one
INSERT INTO meetings (owner_id, title, source, template, hotwords)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMeeting :one
SELECT sqlc.embed(m), u.name AS owner_name
FROM meetings m JOIN users u ON u.id = m.owner_id
WHERE m.id = $1;

-- name: LockMeeting :one
SELECT * FROM meetings WHERE id = $1 FOR UPDATE;

-- name: ListMeetings :many
-- 一期可见范围固定为团队，visibility 先存着，二期按它过滤。
-- query 是 Go 里拼好的 tsquery（见 search.TSQuery），为空表示不筛选。
-- owner 不为空时只列这个人创建的。
-- before_at、before_id 用来翻页：只列排在这场会议后面的（搜索时一页一页取候选）。
SELECT sqlc.embed(m), u.name AS owner_name
FROM meetings m
JOIN users u ON u.id = m.owner_id
LEFT JOIN meeting_search s ON s.meeting_id = m.id
WHERE m.deleted_at IS NULL
  AND (sqlc.narg('owner')::uuid IS NULL OR m.owner_id = sqlc.narg('owner')::uuid)
  AND (sqlc.narg('query')::text IS NULL OR s.doc @@ (sqlc.narg('query')::text)::tsquery)
  AND (sqlc.narg('before_at')::timestamptz IS NULL
       OR (m.created_at, m.id) < (sqlc.narg('before_at')::timestamptz, sqlc.narg('before_id')::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg('max_rows');

-- name: SetMeetingSearch :exec
-- 直接用分好的词建 tsvector，不经过 Postgres 的分词器（它对中文的处理取决于数据库 locale）
INSERT INTO meeting_search (meeting_id, doc) VALUES ($1, array_to_tsvector(sqlc.arg('terms')::text[]))
ON CONFLICT (meeting_id) DO UPDATE SET doc = EXCLUDED.doc;

-- name: RenameMeeting :one
UPDATE meetings SET title = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: AddChunk :exec
INSERT INTO upload_chunks (meeting_id, seq, size) VALUES ($1, $2, $3)
ON CONFLICT (meeting_id, seq) DO UPDATE SET size = EXCLUDED.size;

-- name: TouchChunkTime :exec
UPDATE meetings SET last_chunk_at = now() WHERE id = $1;

-- name: SetMeetingMarks :execrows
-- 存下录音时标记的重点。只在还没交给后台处理时能改
UPDATE meetings SET marks = $2, updated_at = now()
WHERE id = $1 AND status = 'uploading';

-- name: ListChunks :many
SELECT seq, size FROM upload_chunks WHERE meeting_id = $1 ORDER BY seq;

-- name: DeleteChunks :exec
DELETE FROM upload_chunks WHERE meeting_id = $1;

-- name: QueueMeeting :one
-- 入队：换一个新的 run_id。from_status 限定允许从哪些状态入队。
UPDATE meetings
SET status = 'queued', stage = '', progress = 0, error = '',
    run_id = sqlc.arg('run_id'), run_by = sqlc.narg('run_by'), heartbeat_at = NULL, requeues = 0,
    template = coalesce(sqlc.narg('template'), template),
    updated_at = now()
WHERE id = sqlc.arg('id') AND status = ANY(sqlc.arg('from_status')::text[])
RETURNING *;

-- name: ClaimMeeting :one
-- worker 开始处理。同一个 run 被 River 重试时状态已经是 processing，也允许。
UPDATE meetings SET status = 'processing', heartbeat_at = now(), updated_at = now()
WHERE id = $1 AND run_id = $2 AND status IN ('queued', 'processing')
RETURNING *;

-- name: Heartbeat :execrows
UPDATE meetings SET heartbeat_at = now()
WHERE id = $1 AND run_id = $2 AND status = 'processing';

-- name: SetMeetingProgress :execrows
UPDATE meetings SET stage = $3, progress = $4, heartbeat_at = now(), updated_at = now()
WHERE id = $1 AND run_id = $2 AND status = 'processing';

-- name: SetMeetingAudio :execrows
UPDATE meetings SET audio_key = $3, duration_sec = $4, updated_at = now()
WHERE id = $1 AND run_id = $2 AND status = 'processing';

-- name: FailMeeting :execrows
UPDATE meetings SET status = 'failed', error = $3, updated_at = now()
WHERE id = $1 AND run_id = $2 AND status = 'processing';

-- name: FinishMeeting :execrows
UPDATE meetings SET status = 'done', stage = '', progress = 1, error = '', updated_at = now()
WHERE id = $1 AND run_id = $2 AND status = 'processing';

-- name: FailStalled :execrows
-- worker 反复异常退出的会议不再重试
UPDATE meetings SET status = 'failed', error = sqlc.arg('error'), updated_at = now()
WHERE status = 'processing'
  AND heartbeat_at < now() - make_interval(secs => sqlc.arg('stale_secs')::int)
  AND requeues >= sqlc.arg('max_requeues')::int;

-- name: RequeueStalled :many
-- 心跳停了的会议换一个 run_id 重新入队；旧任务即使还活着也写不进去了
UPDATE meetings
SET status = 'queued', run_id = gen_random_uuid(), heartbeat_at = NULL,
    stage = '', progress = 0, requeues = requeues + 1, updated_at = now()
WHERE status = 'processing'
  AND heartbeat_at < now() - make_interval(secs => sqlc.arg('stale_secs')::int)
  AND requeues < sqlc.arg('max_requeues')::int
RETURNING id, run_id;

-- name: SaveTranscript :exec
INSERT INTO transcripts (meeting_id, segments) VALUES ($1, $2)
ON CONFLICT (meeting_id) DO UPDATE SET segments = EXCLUDED.segments, created_at = now();

-- name: GetTranscript :one
SELECT segments FROM transcripts WHERE meeting_id = $1;

-- name: AddMinutesVersion :one
-- 版本号取 max+1，并把会议指向新版本。调用前先 LockMeeting，避免并发时撞号。
WITH v AS (
    INSERT INTO minutes_versions (meeting_id, version, content, source, template, author_id)
    VALUES ($1,
            (SELECT coalesce(max(mv.version), 0) + 1 FROM minutes_versions mv WHERE mv.meeting_id = $1),
            $2, $3, $4, $5)
    RETURNING meeting_id, version
)
UPDATE meetings m SET current_version = v.version, updated_at = now()
FROM v WHERE m.id = v.meeting_id
RETURNING v.version;

-- name: GetMinutesVersion :one
SELECT * FROM minutes_versions WHERE meeting_id = $1 AND version = $2;

-- name: ListMinutesVersions :many
SELECT mv.version, mv.source, mv.template, mv.created_at, coalesce(u.name, '') AS author
FROM minutes_versions mv LEFT JOIN users u ON u.id = mv.author_id
WHERE mv.meeting_id = $1
ORDER BY mv.version DESC;

-- name: SetMeetingTerms :exec
-- 写完纪要后存下大模型给出的术语表
UPDATE meetings SET terms = $2 WHERE id = $1;

-- name: TermsMatching :many
-- 全部会议的术语表里，正确写法或者听错写法就是 q 的那些条目（不分大小写、不管空格：「Go 语言」和「Go语言」算一样）。
-- 搜索时用来扩展搜索词
SELECT DISTINCT (t.value->>'term')::text AS term, (t.value->'variants')::jsonb AS variants
FROM meetings m, jsonb_array_elements(m.terms) AS t
WHERE m.deleted_at IS NULL
  AND (regexp_replace(lower(t.value->>'term'), '\s+', '', 'g') = regexp_replace(lower(sqlc.arg('q')::text), '\s+', '', 'g')
   OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(t.value->'variants') AS v
              WHERE regexp_replace(lower(v), '\s+', '', 'g') = regexp_replace(lower(sqlc.arg('q')::text), '\s+', '', 'g')));

-- name: TranscriptsFor :many
-- 一批会议的转写，搜索时找命中的句子用
SELECT meeting_id, segments FROM transcripts WHERE meeting_id = ANY(sqlc.arg('ids')::uuid[]);

-- name: CurrentMinutesFor :many
-- 一批会议当前版本的纪要，搜索时找命中的行用
SELECT mv.meeting_id, mv.content
FROM meetings m JOIN minutes_versions mv ON mv.meeting_id = m.id AND mv.version = m.current_version
WHERE m.id = ANY(sqlc.arg('ids')::uuid[]);

-- name: MarkRetranscribe :exec
-- 要求后台从录音重新识别。旧的转写先留着，新的存好时（同一个事务里）才清掉这个标记
UPDATE meetings SET retranscribe = true WHERE id = $1;

-- name: ClearRetranscribe :exec
UPDATE meetings SET retranscribe = false WHERE id = $1;

-- name: TrashMeeting :one
-- 放进回收站
UPDATE meetings SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreMeeting :one
-- 从回收站恢复
UPDATE meetings SET deleted_at = NULL, deleted_by = NULL
WHERE id = $1 AND deleted_at IS NOT NULL
RETURNING *;

-- name: PurgeMeeting :one
-- 彻底删除回收站里的一场会议（转写、纪要、分片记录跟着级联删掉），返回录音文件的位置，由调用方删文件
DELETE FROM meetings WHERE id = $1 AND deleted_at IS NOT NULL
RETURNING audio_key;

-- name: ListTrash :many
-- 回收站里的会议，最近删的在前。owner 不为空时只列这个人创建的
SELECT sqlc.embed(m), u.name AS owner_name, coalesce(d.name, '')::text AS deleted_by_name
FROM meetings m
JOIN users u ON u.id = m.owner_id
LEFT JOIN users d ON d.id = m.deleted_by
WHERE m.deleted_at IS NOT NULL
  AND (sqlc.narg('owner')::uuid IS NULL OR m.owner_id = sqlc.narg('owner')::uuid)
ORDER BY m.deleted_at DESC;

-- name: ExpiredTrash :many
-- 在回收站里放了超过 days 天的会议
SELECT id FROM meetings
WHERE deleted_at < now() - make_interval(days => sqlc.arg('days')::int)
ORDER BY deleted_at
LIMIT 100;

-- name: SetMeetingSpeakers :exec
UPDATE meetings SET speakers = $2, updated_at = now() WHERE id = $1;
