-- +goose Up

-- 记住说话人（声纹）。管理员在设置里打开以后才收集（settings 表里的 voiceprints），默认关着：声音特征属于生物特征信息。
-- 声音特征是说话人模型（CAM++）从一段说话里算出的一串数字（长度归一化成 1），只存在这台服务器上，不能还原成声音。

-- 每场会议里每位说话人的声音特征，每次转写（包括重新转写）整场替换。
-- 有人给说话人改名时从这里取，记到声纹库里。说话太少的人没有
CREATE TABLE speaker_embeddings (
    meeting_id uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    speaker    integer NOT NULL,
    embedding  real[] NOT NULL,
    -- 用了多少秒的说话算出来的
    seconds    real NOT NULL,
    PRIMARY KEY (meeting_id, speaker)
);

-- 声纹库：一个人一条，名字不分大小写不能重复。这个人的声纹是他所有样本的平均方向，用到时再算
CREATE TABLE voiceprints (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- 最近一次加样本、去掉样本的时间
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX voiceprints_name_key ON voiceprints (lower(name));

-- 声纹的样本：有人在会议里把某位说话人改成这个名字时记一条（从 speaker_embeddings 复制过来）。
-- 同一场会议的同一位说话人只有一条，改成别的名字时挪到那个人名下，去掉名字时删掉。按声音自动起的名字不记
CREATE TABLE voiceprint_samples (
    meeting_id    uuid NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    speaker       integer NOT NULL,
    voiceprint_id uuid NOT NULL REFERENCES voiceprints (id) ON DELETE CASCADE,
    embedding     real[] NOT NULL,
    seconds       real NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meeting_id, speaker)
);
CREATE INDEX voiceprint_samples_voiceprint ON voiceprint_samples (voiceprint_id);

-- 哪几位说话人（编号）的名字是按声音自动起的。网页上标「按声音认出」；有人改过名以后就不算了
ALTER TABLE meetings ADD COLUMN auto_speakers integer[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE meetings DROP COLUMN auto_speakers;
DROP TABLE voiceprint_samples;
DROP TABLE voiceprints;
DROP TABLE speaker_embeddings;
