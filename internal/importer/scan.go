// Package importer 实现批量导入的扫描与分组逻辑：递归扫描目录、读取 EXIF
// 拍摄时间、把照片归入时间轴节点。纯函数、不碰网络，便于单测。
package importer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"

	"shiguang/internal/imgproc"
)

// GroupMode 决定照片如何归入节点。
type GroupMode string

const (
	// GroupAuto：位于子目录中的照片按子目录（相对根的第一层）归组，
	// 直接躺在根目录的按 EXIF 拍摄日期归组。最贴合"部分整理过"的照片库。
	GroupAuto GroupMode = "auto"
	// GroupDate：一律按 EXIF 拍摄日期归组（无 EXIF 时退回文件修改时间）。
	GroupDate GroupMode = "date"
	// GroupFolder：一律按第一层子目录归组，根目录散图归入一个"未分类"节点。
	GroupFolder GroupMode = "folder"
	// GroupSingle：全部放进同一个节点。
	GroupSingle GroupMode = "single"
)

// ParseGroupMode 解析命令行传入的分组方式。
func ParseGroupMode(s string) (GroupMode, error) {
	switch GroupMode(s) {
	case GroupAuto, GroupDate, GroupFolder, GroupSingle:
		return GroupMode(s), nil
	}
	return "", fmt.Errorf("未知的分组方式 %q（可选 auto|date|folder|single）", s)
}

// Photo 是扫描到的一张待导入照片。
type Photo struct {
	Path    string    // 绝对路径
	Rel     string    // 相对扫描根的路径
	Name    string    // 文件名
	Size    int64     //
	Ext     string    // jpg|png|webp（按魔数判定，不信任扩展名）
	TakenAt time.Time // EXIF 拍摄时间；取不到时为文件修改时间
	HasEXIF bool      // TakenAt 是否来自 EXIF
	// Caption 来自同目录清单文件；为空时上传阶段回落到文件名（去扩展名）。
	Caption string
}

// Group 是一个待创建/复用的时间轴节点及其照片。
type Group struct {
	Date        string // YYYY-MM-DD，取组内最早的拍摄时间
	Title       string
	Description string // 仅来自清单文件；否则为空
	Photos      []*Photo
	// DateFromEXIF 表示决定 Date 的那张照片是否带 EXIF 拍摄时间。
	// 为 false 说明日期来自文件修改时间——扫描的老照片、被聊天软件转发过的
	// 图片都属此类，日期通常与实际拍摄日无关，导入后需要人工核对。
	DateFromEXIF bool
	// DateFromManifest 表示日期由清单文件显式指定，此时无需再提示可信度。
	DateFromManifest bool
}

// EXIFCount 返回组内带 EXIF 拍摄时间的照片数。
func (g *Group) EXIFCount() int {
	n := 0
	for _, p := range g.Photos {
		if p.HasEXIF {
			n++
		}
	}
	return n
}

// skipDir 是扫描时跳过的目录名（系统缩略图/元数据目录）。
var skipDir = map[string]bool{
	".git": true, "@eaDir": true, ".thumbnails": true, ".Trash": true,
	"$RECYCLE.BIN": true, "System Volume Information": true,
}

// Scan 递归扫描 root，返回全部可导入的照片（按相对路径排序，保证可重现）。
// 通过魔数而非扩展名判定类型：改名的 .txt 在这一步就被剔除，不会浪费一次上传。
func Scan(root string, maxBytes int64) ([]*Photo, []string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("importer: 解析路径: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("importer: 打开目录: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("importer: %s 不是目录", absRoot)
	}

	var photos []*Photo
	var skipped []string
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s（无法访问：%v）", p, err))
			return nil
		}
		if d.IsDir() {
			if p != absRoot && (skipDir[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s（无法读取信息）", p))
			return nil
		}
		if fi.Size() > maxBytes {
			skipped = append(skipped, fmt.Sprintf("%s（%.1fMB 超过上限）",
				p, float64(fi.Size())/(1<<20)))
			return nil
		}

		ext, takenAt, hasEXIF, err := probe(p)
		if err != nil {
			// 非图片文件静默跳过（目录里混着文档、视频是常态），
			// 只有"看起来像图片却读不了"才提示
			if isImageExt(d.Name()) {
				skipped = append(skipped, fmt.Sprintf("%s（%v）", p, err))
			}
			return nil
		}
		rel, _ := filepath.Rel(absRoot, p)
		if takenAt.IsZero() {
			takenAt = fi.ModTime()
		}
		photos = append(photos, &Photo{
			Path: p, Rel: filepath.ToSlash(rel), Name: d.Name(), Size: fi.Size(),
			Ext: ext, TakenAt: takenAt, HasEXIF: hasEXIF,
		})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("importer: 扫描目录: %w", err)
	}
	sort.Slice(photos, func(i, j int) bool { return photos[i].Rel < photos[j].Rel })
	return photos, skipped, nil
}

func isImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

// probe 读文件头判定类型，并尝试取 EXIF DateTimeOriginal。
// 只读前 64KB —— EXIF 段在文件开头，无需把整张图读进内存。
func probe(path string) (ext string, takenAt time.Time, hasEXIF bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, false, err
	}
	defer f.Close()

	head := make([]byte, 64<<10)
	n, err := f.Read(head)
	if n < 12 {
		return "", time.Time{}, false, fmt.Errorf("文件过小或无法读取")
	}
	head = head[:n]

	ext, err = imgproc.SniffFormat(head[:12])
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("不是 jpg / png / webp")
	}
	if ext == "jpg" {
		if x, e := exif.Decode(newHeadReader(head)); e == nil {
			if dt, e := x.DateTime(); e == nil {
				return ext, dt, true, nil
			}
		}
	}
	return ext, time.Time{}, false, nil
}

// Plan 把扫描结果按 mode 分组；singleTitle 用于 GroupSingle 模式的节点标题。
func Plan(photos []*Photo, mode GroupMode, singleTitle string) []*Group {
	byKey := map[string]*Group{}
	order := []string{}

	add := func(key, date, title string, p *Photo) {
		g, ok := byKey[key]
		if !ok {
			g = &Group{Date: date, Title: title}
			byKey[key] = g
			order = append(order, key)
		}
		g.Photos = append(g.Photos, p)
	}

	for _, p := range photos {
		date := p.TakenAt.Format("2006-01-02")
		topDir := topLevelDir(p.Rel)
		switch mode {
		case GroupSingle:
			add("__single__", date, singleTitle, p)
		case GroupDate:
			add("d:"+date, date, date, p)
		case GroupFolder:
			if topDir == "" {
				add("f:__root__", date, "未分类", p)
			} else {
				add("f:"+topDir, date, topDir, p)
			}
		default: // GroupAuto
			if topDir == "" {
				add("d:"+date, date, date, p)
			} else {
				add("f:"+topDir, date, topDir, p)
			}
		}
	}

	out := make([]*Group, 0, len(order))
	for _, k := range order {
		g := byKey[k]
		// 组内按拍摄时间排序，同时间按文件名，保证节点内顺序稳定且符合直觉
		sort.SliceStable(g.Photos, func(i, j int) bool {
			if !g.Photos[i].TakenAt.Equal(g.Photos[j].TakenAt) {
				return g.Photos[i].TakenAt.Before(g.Photos[j].TakenAt)
			}
			return g.Photos[i].Rel < g.Photos[j].Rel
		})
		// 排序后第一张即最早，节点日期与其可信度都由它决定
		if len(g.Photos) > 0 {
			g.Date = g.Photos[0].TakenAt.Format("2006-01-02")
			g.DateFromEXIF = g.Photos[0].HasEXIF
			if mode == GroupDate {
				g.Title = g.Date
			}
		}
		out = append(out, g)
	}
	// 节点按日期倒序（与时间轴一致），日期相同按标题
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date > out[j].Date
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// ScanAndPlan 是扫描 → 分组 → 套用清单的完整前置流程，dry-run 与真正导入
// 共用同一条路径，保证「预览到什么就导入什么」。
// 返回的 warnings 包含跳过的文件与清单相关提示。
func ScanAndPlan(root string, mode GroupMode, singleTitle string, maxBytes int64) (
	groups []*Group, photos []*Photo, warnings []string, err error) {

	photos, skipped, err := Scan(root, maxBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	warnings = append(warnings, skipped...)
	if len(photos) == 0 {
		return nil, nil, warnings, nil
	}
	groups = Plan(photos, mode, singleTitle)

	manifests, err := LoadManifests(root)
	if err != nil {
		// 清单读不了不该挡住导入，降级为警告
		warnings = append(warnings, fmt.Sprintf("读取清单文件失败：%v", err))
		return groups, photos, warnings, nil
	}
	warnings = append(warnings, ApplyManifests(root, photos, groups, manifests, mode)...)

	// 清单可能改了标题/日期，重新按日期倒序排一次，保持展示与写入顺序一致
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Date != groups[j].Date {
			return groups[i].Date > groups[j].Date
		}
		return groups[i].Title < groups[j].Title
	})
	return groups, photos, warnings, nil
}

// topLevelDir 返回相对路径的第一层目录名；文件直接位于根时返回空串。
func topLevelDir(rel string) string {
	i := strings.IndexByte(rel, '/')
	if i < 0 {
		return ""
	}
	return rel[:i]
}
