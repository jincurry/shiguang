-- +goose Up
-- 信箱：写给以后的人的信。
-- deliver_at 是投递时间——到点之前这封信在前台完全不存在（API 层过滤），
-- 所以可以今天写一封，指定几年后的某天投递。
CREATE TABLE letters (
    id TEXT PRIMARY KEY,                    -- ULID
    title TEXT NOT NULL CHECK(length(title)<=120),
    body TEXT NOT NULL CHECK(length(body)<=8000),
    sender TEXT NOT NULL DEFAULT '' CHECK(length(sender)<=60),
    deliver_at TEXT NOT NULL,               -- 到这个时刻才出现在信箱里
    read_at TEXT,                           -- 首次读取时刻；空 = 未读
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    deleted_at TEXT
);
-- 前台按投递时间倒序取已投递的信；未投递的不该被扫到
CREATE INDEX idx_letters_deliver ON letters(deliver_at DESC)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_letters_deliver;
DROP TABLE letters;
