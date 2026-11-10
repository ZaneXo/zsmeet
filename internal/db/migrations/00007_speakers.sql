-- +goose Up

-- 给说话人起的名字：{"0": "张敏", "1": "王悦"}，键是转写里的说话人编号（从 0 开始）。
-- 转写、搜索结果显示名字，重新生成纪要时也交给大模型
ALTER TABLE meetings ADD COLUMN speakers jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE meetings DROP COLUMN speakers;
