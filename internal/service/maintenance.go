package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"shiguang/internal/blob"
	"shiguang/internal/store"
)

// RecoverStuck 把卡住的照片重新入队：
// processing 超过 staleAfter（启动时传 0 表示全部——崩溃遗留必然是孤儿），
// 以及 sha 已就绪却仍 pending 且无在途会话的记录。
func (s *Service) RecoverStuck(ctx context.Context, staleAfter time.Duration) (int, error) {
	cutoff := store.FormatTime(time.Now().Add(-staleAfter))
	ids, err := s.st.StuckPhotos(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if s.pool.Enqueue(id) {
			n++
		}
	}
	if n > 0 {
		s.log.Info("recovered stuck photos", "count", n)
	}
	return n, nil
}

// Reap 清理过期上传会话：session→expired、photo→failed("上传未完成")、删 staging。
func (s *Service) Reap(ctx context.Context) (int, error) {
	sessions, err := s.st.ExpiredSessions(ctx, store.Now())
	if err != nil {
		return 0, err
	}
	for _, sess := range sessions {
		ok, err := s.st.SetSessionState(ctx, sess.ID, "expired")
		if err != nil || !ok {
			continue // 竞争中被 confirm 走了，跳过
		}
		if err := s.st.MarkPhotoFailed(ctx, sess.PhotoID, "上传未完成", store.Now()); err != nil {
			s.log.Error("reap: mark failed", "photo", sess.PhotoID, "err", err)
		}
		if err := s.bl.Delete(ctx, sess.ObjectKey); err != nil {
			s.log.Error("reap: delete staging", "key", sess.ObjectKey, "err", err)
		}
	}
	if len(sessions) > 0 {
		s.log.Info("reaped expired upload sessions", "count", len(sessions))
	}
	return len(sessions), nil
}

// orphanSeen 记录孤儿候选首见时间（两阶段删除：本次标记、下次超龄才删）。
// 进程重启丢失只会推迟删除，方向安全。
var (
	orphanMu   sync.Mutex
	orphanSeen = map[string]time.Time{}
)

// GC 每日物理清理：
//  1. 软删超期条目 → 删行；删 blob 前必须校验 sha 引用数为 0
//     （同图可被多节点共享；这里比提示词更严格：任何行——含未到期回收站条目——
//     仍引用即保留，否则恢复会断链）；
//  2. blob 全量与 DB 对账，删除超龄孤儿（local 用 mtime>48h，其余驱动用
//     进程内两阶段标记，两次 GC 之间 >48h 才删）。
func (s *Service) GC(ctx context.Context) error {
	cutoff := store.FormatTime(time.Now().Add(-time.Duration(s.cfg.TrashTTLDays) * 24 * time.Hour))

	shas := map[string]bool{} // 待检查 blob 引用的 sha 集合

	// 独立删除的照片
	photos, err := s.st.ListPurgeablePhotos(ctx, cutoff)
	if err != nil {
		return err
	}
	var photoIDs []string
	for _, p := range photos {
		photoIDs = append(photoIDs, p.ID)
		if p.SHA256 != nil {
			shas[*p.SHA256] = true
		}
	}
	if err := s.st.HardDeletePhotos(ctx, photoIDs); err != nil {
		return err
	}

	// 超期节点（连带其全部照片行）
	nodes, err := s.st.ListPurgeableNodes(ctx, cutoff)
	if err != nil {
		return err
	}
	for nodeID, nodePhotos := range nodes {
		var ids []string
		for _, p := range nodePhotos {
			ids = append(ids, p.ID)
			if p.SHA256 != nil {
				shas[*p.SHA256] = true
			}
		}
		if err := s.st.HardDeletePhotos(ctx, ids); err != nil {
			return err
		}
		if err := s.st.HardDeleteNode(ctx, nodeID); err != nil {
			return err
		}
	}

	// 行已删，无引用的 sha 才可删 blob
	for sha := range shas {
		n, err := s.st.CountAnySHARefs(ctx, sha)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		s.deleteBlobsForSHA(ctx, sha)
	}

	if len(photoIDs) > 0 || len(nodes) > 0 {
		s.invalidateStats()
		s.log.Info("gc purged trash", "photos", len(photoIDs), "nodes", len(nodes))
	}

	return s.reconcileOrphans(ctx)
}

// deleteBlobsForSHA 删除某 sha 的原图与全部变体。
func (s *Service) deleteBlobsForSHA(ctx context.Context, sha string) {
	prefix := blob.VariantPrefix(sha)
	s.bl.List(ctx, prefix, func(key string) error {
		if err := s.bl.Delete(ctx, key); err != nil {
			s.log.Error("gc: delete variant", "key", key, "err", err)
		}
		return nil
	})
	// 原图扩展名未知（行已删），按三种候选尝试
	for _, ext := range []string{"jpg", "png", "webp"} {
		key := blob.OrigKey(sha, ext)
		if _, err := s.bl.Stat(ctx, key); err == nil {
			if err := s.bl.Delete(ctx, key); err != nil {
				s.log.Error("gc: delete orig", "key", key, "err", err)
			}
		}
	}
}

// shaFromKey 从 orig/var key 提取 sha256（64 位十六进制），提取不到返回空串。
func shaFromKey(key string) string {
	parts := strings.Split(key, "/")
	// orig/ab/cd/<sha>.<ext> → parts[3]=<sha>.<ext>；var/ab/cd/<sha>/<name>.webp → parts[3]=<sha>
	if len(parts) < 4 {
		return ""
	}
	seg := parts[3]
	if i := strings.IndexByte(seg, '.'); i >= 0 {
		seg = seg[:i]
	}
	if len(seg) != 64 {
		return ""
	}
	return seg
}

// reconcileOrphans 对账：blob 中存在但 DB 无任何行引用的对象，超龄（>48h）删除。
func (s *Service) reconcileOrphans(ctx context.Context) error {
	const orphanAge = 48 * time.Hour
	now := time.Now()
	deleted := 0

	handle := func(key string) error {
		sha := shaFromKey(key)
		if sha == "" {
			return nil
		}
		known, err := s.st.SHAKnown(ctx, sha)
		if err != nil {
			return err
		}
		if known {
			orphanMu.Lock()
			delete(orphanSeen, key)
			orphanMu.Unlock()
			return nil
		}
		// local 驱动有 mtime，直接判龄；其他驱动两阶段标记
		if lm, ok := s.bl.(interface {
			MTime(string) (time.Time, error)
		}); ok {
			if mt, err := lm.MTime(key); err == nil {
				if now.Sub(mt) > orphanAge {
					if err := s.bl.Delete(ctx, key); err == nil {
						deleted++
					}
				}
				return nil
			}
		}
		orphanMu.Lock()
		first, seen := orphanSeen[key]
		if !seen {
			orphanSeen[key] = now
			orphanMu.Unlock()
			return nil
		}
		orphanMu.Unlock()
		if now.Sub(first) > orphanAge {
			if err := s.bl.Delete(ctx, key); err == nil {
				deleted++
				orphanMu.Lock()
				delete(orphanSeen, key)
				orphanMu.Unlock()
			}
		}
		return nil
	}

	for _, prefix := range []string{"orig/", "var/"} {
		if err := s.bl.List(ctx, prefix, handle); err != nil {
			return err
		}
	}
	if deleted > 0 {
		s.log.Info("gc deleted orphan blobs", "count", deleted)
	}
	return nil
}
