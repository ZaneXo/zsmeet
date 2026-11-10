-- +goose Up

-- 会上专有名词的术语表：正确写法和它在转写里被听错的写法，写纪要时由大模型给出。
-- 搜索时用它扩展搜索词：搜「飞书」时，连转写里被听成「非书」的地方也能找到。
-- [{"term": "飞书", "variants": ["非书"]}]
ALTER TABLE meetings ADD COLUMN terms jsonb NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE meetings DROP COLUMN terms;
