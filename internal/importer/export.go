// 导出：把整个相册摊成人类可读的目录树。
//
// 家庭照片的时间尺度是几十年，而这套软件多半活不了那么久。所以导出的东西
// 必须不依赖它：照片是原图，文字就在照片旁边的同名 .txt 里，节点信息用的是
// sgctl import 认识的清单格式——导出的目录能被原样导回来，闭环。
package importer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ExportOptions 控制导出范围与落地位置。
type ExportOptions struct {
	OutDir string // 导出到哪个目录
	From   string // YYYY-MM-DD，含；空表示不限
	To     string // YYYY-MM-DD，含；空表示不限
	DryRun bool   // 只列清单不落盘
}

// ExportStats 是一次导出的结果。
type ExportStats struct {
	Nodes   int
	Photos  int
	Bytes   int64
	Reused  int // 已经下好、这次跳过的
	Skipped int // 尚未处理完（非 ready）的照片
	Elapsed time.Duration
}

// ExportPlanItem 是 dry-run 时列出的一个节点。
type ExportPlanItem struct {
	Dir    string
	Photos int
}

// Export 把相册导出到 opt.OutDir。progress 可为 nil。
func Export(ctx context.Context, c *Client, opt ExportOptions,
	progress func(dir, file string, done, total int)) (*ExportStats, []ExportPlanItem, error) {

	start := time.Now()
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("拉取时间轴失败：%w", err)
	}

	// 时间倒序拉下来的，导出按正序更像一本相册
	kept := make([]*NodeDTO, 0, len(nodes))
	for _, n := range nodes {
		if opt.From != "" && n.Date < opt.From {
			continue
		}
		if opt.To != "" && n.Date > opt.To {
			continue
		}
		kept = append(kept, n)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Date < kept[j].Date })

	st := &ExportStats{}
	plan := make([]ExportPlanItem, 0, len(kept))
	total := 0
	for _, n := range kept {
		total += len(n.Photos)
	}

	used := map[string]bool{} // 目录重名消歧
	done := 0
	for _, n := range kept {
		dir := uniqueName(used, nodeDirName(n))
		plan = append(plan, ExportPlanItem{Dir: dir, Photos: len(n.Photos)})
		if opt.DryRun {
			st.Nodes++
			st.Photos += len(n.Photos)
			continue
		}

		full := filepath.Join(opt.OutDir, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			return nil, nil, fmt.Errorf("建目录 %s：%w", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, ManifestName),
			[]byte(nodeManifest(n)), 0o644); err != nil {
			return nil, nil, fmt.Errorf("写 %s：%w", ManifestName, err)
		}
		st.Nodes++

		names := map[string]bool{}
		for i, p := range n.Photos {
			if p.Status != "ready" {
				st.Skipped++
				done++
				continue
			}
			base := uniqueName(names, photoBaseName(i+1, p))
			// 已经下好的就跳过：几万张的导出中途断了，重跑不该从头再来。
			// 内容寻址保证同一张照片的字节数不会变，大小一致即认定完整。
			if p.SizeBytes != nil {
				if fi, err := os.Stat(filepath.Join(full, base)); err == nil &&
					fi.Size() == *p.SizeBytes {
					if err := writePhotoText(full, base, p); err != nil {
						return nil, nil, err
					}
					st.Reused++
					st.Photos++
					done++
					continue
				}
			}
			if progress != nil {
				progress(dir, base, done, total)
			}
			nBytes, err := writePhoto(ctx, c, full, base, p)
			if err != nil {
				return nil, nil, err
			}
			st.Bytes += nBytes
			st.Photos++
			done++
		}
	}
	if !opt.DryRun {
		if err := os.WriteFile(filepath.Join(opt.OutDir, "这份导出是什么.txt"),
			[]byte(readmeText(st, time.Now())), 0o644); err != nil {
			return nil, nil, err
		}
	}
	st.Elapsed = time.Since(start)
	return st, plan, nil
}

// writePhoto 落地一张照片：原图 + 同名 .txt（有话可说时才写）。
func writePhoto(ctx context.Context, c *Client, dir, base string, p *PhotoDTO) (int64, error) {
	tmp := filepath.Join(dir, "."+base+".part")
	var n int64
	// 导出是密集的连续请求，必然踩到服务端限流。429 是协作式背压不是失败，
	// withRetry 会按 Retry-After 等待且不消耗错误预算；每次重试都重开临时
	// 文件，免得把两次的字节接在一起。
	err := withRetry(ctx, 4, func() error {
		f, err := os.Create(tmp)
		if err != nil {
			return fmt.Errorf("建文件 %s：%w", tmp, err)
		}
		written, derr := c.DownloadOriginal(ctx, p.ID, f)
		cerr := f.Close()
		if derr != nil {
			os.Remove(tmp)
			return derr
		}
		if cerr != nil {
			os.Remove(tmp)
			return cerr
		}
		n = written
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("下载 %s：%w", base, err)
	}
	// 先下完再改名：中途断了不会留下一个看着像照片的半截文件
	if err := os.Rename(tmp, filepath.Join(dir, base)); err != nil {
		return 0, err
	}
	if err := writePhotoText(dir, base, p); err != nil {
		return 0, err
	}
	return n, nil
}

// writePhotoText 写照片旁边那份文字；没话可说时不留空文件，
// 之前写过而现在被清空了则删掉，免得留下过期的字。
func writePhotoText(dir, base string, p *PhotoDTO) error {
	name := filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+".txt")
	txt := photoText(p)
	if txt == "" {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(name, []byte(txt), 0o644)
}

// nodeDirName 生成「2026-08-03 黄山」这样的目录名。
func nodeDirName(n *NodeDTO) string {
	title := SafeName(n.Title)
	if title == "" {
		title = "未命名"
	}
	return n.Date + " " + title
}

// extRe 匹配像样的扩展名：点加 1-4 位字母数字。
var extRe = regexp.MustCompile(`^\.[a-z0-9]{1,4}$`)

// photoBaseName 生成「01 云海翻涌.jpg」这样的文件名，序号保证相册内的顺序。
// 图注本身就是文件名时（批量导入默认拿文件名当图注），沿用它原来的扩展名。
func photoBaseName(idx int, p *PhotoDTO) string {
	stem, ext := p.Caption, ".jpg"
	if i := strings.LastIndex(p.Caption, "."); i > 0 && len(p.Caption)-i <= 5 {
		// 扩展名不能走 SafeName：它会把开头那个点一起 trim 掉
		if e := strings.ToLower(p.Caption[i:]); extRe.MatchString(e) {
			ext, stem = e, p.Caption[:i]
		}
	}
	name := SafeName(stem)
	if name == "" {
		name = p.ID
	}
	return fmt.Sprintf("%02d %s%s", idx, name, ext)
}

// nodeManifest 用 sgctl import 认识的清单格式写节点信息，导出目录因此能被导回。
func nodeManifest(n *NodeDTO) string {
	var b strings.Builder
	b.WriteString("# 这个文件夹是「拾光集」的一个时间轴节点。\n")
	b.WriteString("# 下面几行 sgctl import 能直接读回去，改了也算数。\n\n")
	fmt.Fprintf(&b, "%s = %s\n", keyDate, n.Date)
	fmt.Fprintf(&b, "%s = %s\n", keyTitle, oneLine(n.Title))
	if n.Place != "" {
		fmt.Fprintf(&b, "place = %s\n", oneLine(n.Place))
	}
	if n.Description != "" {
		fmt.Fprintf(&b, "%s = %s\n", keyDesc, oneLine(n.Description))
	}
	b.WriteString("\n# 每张照片的图注写在它自己的同名 .txt 里。\n")
	return b.String()
}

// photoText 是照片旁边那份文字：图注、拍摄时间、背面手记。
func photoText(p *PhotoDTO) string {
	var b strings.Builder
	if p.Caption != "" {
		b.WriteString(p.Caption)
		b.WriteString("\n")
	}
	if p.TakenAt != nil && *p.TakenAt != "" {
		fmt.Fprintf(&b, "\n拍摄时间：%s\n", (*p.TakenAt)[:min(10, len(*p.TakenAt))])
	}
	if p.Note != "" {
		b.WriteString("\n--- 相纸背面 ---\n")
		b.WriteString(p.Note)
		b.WriteString("\n")
	}
	return b.String()
}

func readmeText(st *ExportStats, at time.Time) string {
	return fmt.Sprintf(`这份导出是什么
================

导出时间：%s
内容：%d 个日子，%d 张照片

目录怎么读
----------
每个文件夹是一个日子，名字是「日期 标题」。文件夹里：

  shiguang.txt      这个日子的日期、标题、地点、描述
  01 xxx.jpg        照片原图，序号即相册里的顺序
  01 xxx.txt        这张照片的图注、拍摄时间，以及写在相纸背面的话
                    （没写过字的照片不会有这个文件）

照片是原图，不是压缩过的网页版本，用任何看图软件都能打开。
文字是纯文本，用任何编辑器都能打开。

想放回去
--------
这些文件夹可以直接被 sgctl import 读回拾光集：

  sgctl import --group folder ./这个目录/*

shiguang.txt 里的日期与标题会被沿用，改了也算数。
`, at.Format("2006-01-02 15:04"), st.Nodes, st.Photos)
}

// uniqueName 同名时追加 (2)(3)…，避免两个同名节点互相覆盖。
func uniqueName(used map[string]bool, name string) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		try := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !used[try] {
			used[try] = true
			return try
		}
	}
}

// SafeName 把任意文本收拾成能落地的文件名片段：
// 去掉路径分隔符与控制字符，保留中文，按字符数（而非字节）截断。
func SafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteRune(' ')
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.Trim(out, ". ") // Windows 不接受结尾的点或空格
	r := []rune(out)
	if len(r) > 60 {
		out = strings.TrimRight(string(r[:60]), " ")
	}
	return out
}

// oneLine 把多行文本压成一行：清单是行式格式，换行会切断这一项。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = io.Discard
