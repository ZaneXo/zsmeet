-- name: DeleteSpeakerEmbeddings :exec
DELETE FROM speaker_embeddings WHERE meeting_id = $1;

-- name: AddSpeakerEmbedding :exec
INSERT INTO speaker_embeddings (meeting_id, speaker, embedding, seconds) VALUES ($1, $2, $3, $4);

-- name: GetSpeakerEmbedding :one
SELECT * FROM speaker_embeddings WHERE meeting_id = $1 AND speaker = $2;

-- name: UpsertVoiceprint :one
-- 按名字（不分大小写）找声纹，没有就新建；名字的写法以第一次的为准
INSERT INTO voiceprints (name) VALUES ($1)
ON CONFLICT ((lower(name))) DO UPDATE SET updated_at = now()
RETURNING id;

-- name: TouchVoiceprint :exec
UPDATE voiceprints SET updated_at = now() WHERE id = $1;

-- name: GetVoiceprintSample :one
SELECT * FROM voiceprint_samples WHERE meeting_id = $1 AND speaker = $2;

-- name: SaveVoiceprintSample :exec
-- 同一场会议的同一位说话人只有一个样本：已经有了（记在别人名下）就挪过来
INSERT INTO voiceprint_samples (meeting_id, speaker, voiceprint_id, embedding, seconds)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (meeting_id, speaker) DO UPDATE
SET voiceprint_id = excluded.voiceprint_id, embedding = excluded.embedding, seconds = excluded.seconds, created_at = now();

-- name: DeleteVoiceprintSample :many
DELETE FROM voiceprint_samples WHERE meeting_id = $1 AND speaker = $2 RETURNING voiceprint_id;

-- name: MeetingVoiceprintSamples :many
SELECT * FROM voiceprint_samples WHERE meeting_id = $1 ORDER BY speaker;

-- name: DeleteMeetingVoiceprintSamples :exec
DELETE FROM voiceprint_samples WHERE meeting_id = $1;

-- name: LockVoiceprints :exec
-- 声纹库的锁（事务级的 advisory lock，事务结束自动放开）：改声纹库的事务一个一个来，见 service.LockVoiceprints
SELECT pg_advisory_xact_lock(@key::bigint);

-- name: DeleteEmptyVoiceprints :exec
-- 这个事务动过的声纹（去掉、挪走过样本的）里，一个样本都不剩的删掉。只看 ids 里的，不扫整张表
DELETE FROM voiceprints v
WHERE v.id = ANY(@ids::uuid[])
  AND NOT EXISTS (SELECT 1 FROM voiceprint_samples s WHERE s.voiceprint_id = v.id);

-- name: AllVoiceprintSamples :many
-- 配对时用：每个人的全部样本，按人排在一起
SELECT v.id, v.name, s.embedding
FROM voiceprints v JOIN voiceprint_samples s ON s.voiceprint_id = v.id
ORDER BY v.id;

-- name: ListVoiceprints :many
SELECT v.id, v.name, v.created_at, v.updated_at,
       count(DISTINCT s.meeting_id)::int AS meetings,
       sum(s.seconds)::float8 AS seconds
FROM voiceprints v JOIN voiceprint_samples s ON s.voiceprint_id = v.id
GROUP BY v.id
ORDER BY v.updated_at DESC, v.id;

-- name: DeleteVoiceprint :execrows
DELETE FROM voiceprints WHERE id = $1;

-- name: DeleteAllVoiceprints :exec
DELETE FROM voiceprints;

-- name: DeleteAllSpeakerEmbeddings :exec
DELETE FROM speaker_embeddings;

-- name: SetAutoSpeakers :exec
UPDATE meetings SET auto_speakers = $2 WHERE id = $1;
