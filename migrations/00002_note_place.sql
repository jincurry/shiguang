-- +goose Up
-- 相纸背面的手记：老照片背面用铅笔写的那段话，比 caption 长得多。
ALTER TABLE photos ADD COLUMN note TEXT NOT NULL DEFAULT '' CHECK(length(note)<=2000);
-- 节点地点：手填的地名（"外婆家""黄山"），相册在时间之外的第二条线索。
ALTER TABLE nodes ADD COLUMN place TEXT NOT NULL DEFAULT '' CHECK(length(place)<=80);
-- 按地点串联：同一地点的节点按时间倒序取出。
CREATE INDEX idx_nodes_place ON nodes(place, date DESC) WHERE deleted_at IS NULL AND place <> '';
-- 那年今日：按月日匹配，索引让它不必全表扫。
CREATE INDEX idx_nodes_monthday ON nodes(substr(date, 6)) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_nodes_monthday;
DROP INDEX IF EXISTS idx_nodes_place;
ALTER TABLE nodes DROP COLUMN place;
ALTER TABLE photos DROP COLUMN note;
