-- +goose Up
CREATE TABLE nodes (
    id TEXT PRIMARY KEY,                    -- ULID
    date TEXT NOT NULL,                     -- 'YYYY-MM-DD'
    title TEXT NOT NULL CHECK(length(title)<=120),
    description TEXT NOT NULL DEFAULT '' CHECK(length(description)<=2000),
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    deleted_at TEXT
);
CREATE INDEX idx_nodes_timeline ON nodes(date DESC, id DESC) WHERE deleted_at IS NULL;

CREATE TABLE photos (
    id TEXT PRIMARY KEY,                    -- ULID
    node_id TEXT NOT NULL REFERENCES nodes(id),
    caption TEXT NOT NULL DEFAULT '' CHECK(length(caption)<=200),
    ord INTEGER NOT NULL,                   -- 留缝排序 100,200,300…
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK(status IN ('pending','processing','ready','failed')),
    fail_reason TEXT,
    sha256 TEXT, ext TEXT,
    width INTEGER, height INTEGER,
    blurhash TEXT, dominant TEXT, size_bytes INTEGER,
    taken_at TEXT,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    deleted_at TEXT
);
CREATE INDEX idx_photos_node ON photos(node_id, ord) WHERE deleted_at IS NULL;
CREATE INDEX idx_photos_status ON photos(status) WHERE status IN ('pending','processing');
CREATE UNIQUE INDEX uq_node_sha ON photos(node_id, sha256)
    WHERE deleted_at IS NULL AND sha256 IS NOT NULL;

CREATE TABLE upload_sessions (
    id TEXT PRIMARY KEY, photo_id TEXT NOT NULL REFERENCES photos(id),
    object_key TEXT NOT NULL, expect_size INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'issued'
        CHECK(state IN ('issued','confirmed','expired','aborted')),
    expires_at TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX idx_sessions_reap ON upload_sessions(state, expires_at);

-- +goose Down
DROP TABLE upload_sessions;
DROP TABLE photos;
DROP TABLE nodes;
