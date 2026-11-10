-- name: CreateMeeting :one
INSERT INTO meetings (owner_id, title, source, template, hotwords, visibility)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMeeting :one
SELECT sqlc.embed(m), u.name AS owner_name
FROM meetings m JOIN users u ON u.id = m.owner_id
WHERE m.id = $1;

-- name: LockMeeting :one
SELECT * FROM meetings WHERE id = $1 FOR UPDATE;

-- name: ListMeetings :many
-- 只列 viewer 能看的会议（管理员都能看，见 service.Access）。
-- query 是 Go 里拼好的 tsquery（见 search.TSQuery），为空表示不筛选。
-- owner 不为空时只列这个人创建的。
-- before_at、before_id 用来翻页：只列排在这场会议后面的（搜索时一页一页取候选）。
SELECT sqlc.embed(m), u.name AS owner_name,
       (mm.user_id IS NOT NULL)::bool AS is_member, coalesce(mm.can_edit, false)::bool AS member_can_edit
FROM meetings m
JOIN users u ON u.id = m.owner_id
LEFT JOIN meeting_search s ON s.meeting_id = m.id
LEFT JOIN meeting_members mm ON mm.meeting_id = m.id AND mm.user_id = sqlc.arg('viewer')::uuid
WHERE m.deleted_at IS NULL
  AND (sqlc.arg('is_admin')::bool OR m.owner_id = sqlc.arg('viewer')::uuid OR m.visibility = 'team'
       OR (m.visibility = 'members' AND mm.user_id IS NOT NULL))
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
-- 顺带清掉「要重新识别」的标记：重试、重新生成纪要只用现有的转写（重新转写在同一个事务里再标上）。
-- 不然重新识别失败（比如录音没了）以后，每次重试都会再识别一遍、再失败，纪要也一直改不了
UPDATE meetings
SET status = 'queued', stage = '', progress = 0, error = '', retranscribe = false,
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
-- viewer 能看的会议的术语表里，正确写法或者听错写法就是 q 的那些条目（不分大小写、不管空格：「Go 语言」和「Go语言」算一样）。
-- 搜索时用来扩展搜索词。看不到的会议的术语表不用：里面的人名、项目名也算会议内容
SELECT DISTINCT (t.value->>'term')::text AS term, (t.value->'variants')::jsonb AS variants
FROM meetings m
LEFT JOIN meeting_members mm ON mm.meeting_id = m.id AND mm.user_id = sqlc.arg('viewer')::uuid
CROSS JOIN LATERAL jsonb_array_elements(m.terms) AS t
WHERE m.deleted_at IS NULL
  AND (sqlc.arg('is_admin')::bool OR m.owner_id = sqlc.arg('viewer')::uuid OR m.visibility = 'team'
       OR (m.visibility = 'members' AND mm.user_id IS NOT NULL))
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
-- 彻底删除回收站里的一场会议（转写、纪要、分片记录、发到飞书的记录跟着级联删掉），返回录音文件的位置，由调用方删文件。
-- 从飞书会议导入的，导入记录留着（同一场会议不会再被导入一次），但会议主题清掉
WITH gone AS (
    DELETE FROM meetings WHERE meetings.id = $1 AND meetings.deleted_at IS NOT NULL
    RETURNING meetings.id, meetings.audio_key
), imported AS (
    SELECT feishu_imports.topic FROM feishu_imports WHERE feishu_imports.meeting_id IN (SELECT gone.id FROM gone) AND feishu_imports.topic <> ''
), forget AS (
    UPDATE feishu_imports SET topic = '', detail = '', updated_at = now()
    WHERE feishu_imports.meeting_id IN (SELECT gone.id FROM gone)
), forget_note AS (
    -- 设置页面上「最近一次」提到这场会议的话，也不再写主题
    UPDATE feishu_link SET last_event = '一场飞书会议的录制导入以后被彻底删除了'
    WHERE EXISTS (SELECT 1 FROM imported WHERE strpos(feishu_link.last_event, imported.topic) > 0)
)
SELECT gone.audio_key FROM gone;

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

-- name: ExpiredAudio :many
-- 录音过了保留期的会议（创建满 days 天、录音还在、没在处理的），一次最多 100 场
SELECT id, audio_key FROM meetings
WHERE audio_key <> '' AND audio_deleted_at IS NULL AND status IN ('done', 'failed')
  AND created_at < now() - make_interval(days => sqlc.arg('days')::int)
ORDER BY created_at
LIMIT 100;

-- name: MarkAudioDeleted :execrows
-- 记下录音已经删除。会议这时又开始处理（重新转写）的话不删
UPDATE meetings SET audio_deleted_at = now(), updated_at = now()
WHERE id = $1 AND audio_deleted_at IS NULL AND status IN ('done', 'failed');
