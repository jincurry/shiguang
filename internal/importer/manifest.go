package importer

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestName 是放在照片目录里的可选清单文件名。存在时用于覆盖自动推导出的
// 节点信息与图注；不存在时导入行为与没有清单完全一致。
const ManifestName = "shiguang.txt"

// 清单里的保留键（大小写不敏感）。其余行一律按「文件名 = 图注」解析。
// 照片文件都带扩展名，与这三个键天然不冲突。
const (
	keyDate  = "date"
	keyTitle = "title"
	keyDesc  = "description"
)

// Manifest 是一个目录下的清单内容。
type Manifest struct {
	Dir         string            // 清单所在目录（绝对路径）
	Path        string            // 清单文件绝对路径
	Date        string            // 空 = 不覆盖
	Title       string            // 空 = 不覆盖
	Description string            // 空 = 不覆盖
	Captions    map[string]string // 小写文件名 → 图注
	Warnings    []string
}

// Caption 按文件名取图注（大小写不敏感，兼容 Windows/macOS 的大小写不敏感文件系统）。
func (m *Manifest) Caption(filename string) (string, bool) {
	if m == nil {
		return "", false
	}
	c, ok := m.Captions[strings.ToLower(filename)]
	return c, ok
}

// ParseManifest 解析清单内容。格式刻意做得极简，便于手工编辑中文：
//
//	# 以 # 开头的行是注释
//	date = 2019-10-06
//	title = 小妹的婚礼
//	description = 全家去了三亚
//	DSC1000.jpg = 接亲那天早上
//
// 值取到行尾，不需要引号与转义；键与值之间的空白会被去掉。
func ParseManifest(path string, r fs.File) (*Manifest, error) {
	m := &Manifest{
		Path:     path,
		Dir:      filepath.Dir(path),
		Captions: map[string]string{},
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // 去 BOM，Windows 记事本会写
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		i := strings.IndexByte(trimmed, '=')
		if i < 0 {
			m.Warnings = append(m.Warnings,
				fmt.Sprintf("%s 第 %d 行缺少「=」，已忽略：%s", filepath.Base(path), lineNo, trimmed))
			continue
		}
		key := strings.TrimSpace(trimmed[:i])
		val := strings.TrimSpace(trimmed[i+1:])
		if key == "" {
			m.Warnings = append(m.Warnings,
				fmt.Sprintf("%s 第 %d 行没有键名，已忽略", filepath.Base(path), lineNo))
			continue
		}

		switch strings.ToLower(key) {
		case keyDate:
			if val == "" {
				continue
			}
			if _, err := time.Parse("2006-01-02", val); err != nil {
				m.Warnings = append(m.Warnings,
					fmt.Sprintf("%s 第 %d 行日期 %q 不是 YYYY-MM-DD 格式，已忽略", filepath.Base(path), lineNo, val))
				continue
			}
			m.Date = val
		case keyTitle:
			if len([]rune(val)) > 120 {
				m.Warnings = append(m.Warnings,
					fmt.Sprintf("%s 第 %d 行标题超过 120 字，已截断", filepath.Base(path), lineNo))
				val = clampRunes(val, 120)
			}
			m.Title = val
		case keyDesc:
			if len([]rune(val)) > 2000 {
				m.Warnings = append(m.Warnings,
					fmt.Sprintf("%s 第 %d 行描述超过 2000 字，已截断", filepath.Base(path), lineNo))
				val = clampRunes(val, 2000)
			}
			m.Description = val
		default:
			// 键按首个 = 切分。照片文件名必带扩展名，没有扩展名说明多半是
			// 文件名里本身含 =（如「照片 = 1.jpg」）被切错了——这种情况静默
			// 切错比报错更糟，明确提示用户改名。
			if filepath.Ext(key) == "" {
				m.Warnings = append(m.Warnings, fmt.Sprintf(
					"%s 第 %d 行的 %q 不像文件名（缺扩展名），已忽略；"+
						"若文件名中含「=」请先改名——清单按首个「=」切分键值",
					filepath.Base(path), lineNo, key))
				continue
			}
			if len([]rune(val)) > 200 {
				m.Warnings = append(m.Warnings,
					fmt.Sprintf("%s 第 %d 行图注超过 200 字，已截断", filepath.Base(path), lineNo))
				val = clampRunes(val, 200)
			}
			m.Captions[strings.ToLower(key)] = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取清单 %s: %w", path, err)
	}
	return m, nil
}

// LoadManifests 扫描 root 下所有 shiguang.txt，按所在目录（绝对路径）索引。
func LoadManifests(root string) (map[string]*Manifest, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	out := map[string]*Manifest{}
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 无法访问的目录在 Scan 阶段已报告
		}
		if d.IsDir() {
			if p != absRoot && (skipDir[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), ManifestName) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		m, err := ParseManifest(p, f)
		if err != nil {
			return err
		}
		out[filepath.Dir(p)] = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyManifests 把清单套用到扫描结果与分组上，返回按目录排序的警告列表。
//
// 规则（刻意保持可一句话说清）：
//   - 图注：清单所在目录内的照片，按文件名匹配，任何分组方式下都生效；
//   - 节点的 date / title / description：仅当该目录恰好对应一个节点时生效
//     （auto / folder 模式下的第一层子目录，或 single 模式下的根目录），
//     否则忽略并给出警告——否则同一个节点会被多份清单争抢。
func ApplyManifests(root string, photos []*Photo, groups []*Group,
	manifests map[string]*Manifest, mode GroupMode) []string {

	if len(manifests) == 0 {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil
	}

	var warnings []string
	for _, m := range manifests {
		warnings = append(warnings, m.Warnings...)
	}

	// 1) 图注：按照片所在目录找清单
	used := map[string]map[string]bool{} // 清单目录 → 已命中的文件名
	for _, p := range photos {
		dir := filepath.Dir(p.Path)
		m := manifests[dir]
		if m == nil {
			continue
		}
		if c, ok := m.Caption(p.Name); ok {
			p.Caption = c
			if used[dir] == nil {
				used[dir] = map[string]bool{}
			}
			used[dir][strings.ToLower(p.Name)] = true
		}
	}
	// 清单里写了但目录下没有的文件名，多半是拼错，值得提醒
	for dir, m := range manifests {
		for name := range m.Captions {
			if !used[dir][name] {
				warnings = append(warnings,
					fmt.Sprintf("%s 中的 %q 在该目录下找不到对应照片", relOrAbs(absRoot, m.Path), name))
			}
		}
	}

	// 2) 节点字段：先算出每个节点的"锚点目录"
	anchors := groupAnchors(absRoot, groups, mode)
	claimed := map[string]bool{}
	for i, g := range groups {
		dir := anchors[i]
		if dir == "" {
			continue
		}
		m := manifests[dir]
		if m == nil {
			continue
		}
		claimed[dir] = true
		if m.Title != "" {
			g.Title = m.Title
		}
		if m.Date != "" {
			g.Date = m.Date
			g.DateFromManifest = true
		}
		if m.Description != "" {
			g.Description = m.Description
		}
	}
	// 有节点级字段、但目录并不唯一对应一个节点 → 明确告诉用户没生效
	for dir, m := range manifests {
		if claimed[dir] {
			continue
		}
		if m.Date == "" && m.Title == "" && m.Description == "" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s 里的 date/title/description 未生效：当前 --group %s 下该目录不单独对应一个节点（图注仍然生效）",
			relOrAbs(absRoot, m.Path), mode))
	}

	sort.Strings(warnings)
	return warnings
}

// groupAnchors 返回每个节点对应的"锚点目录"绝对路径；无唯一对应目录时为空串。
func groupAnchors(absRoot string, groups []*Group, mode GroupMode) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		switch mode {
		case GroupSingle:
			out[i] = absRoot
		case GroupAuto, GroupFolder:
			// 组内照片必须全部来自同一个第一层目录，该目录才是锚点
			dir := ""
			ok := true
			for _, p := range g.Photos {
				top := topLevelDir(p.Rel)
				if top == "" { // 根目录散图：auto 按日期成组、folder 归「未分类」
					ok = false
					break
				}
				if dir == "" {
					dir = top
				} else if dir != top {
					ok = false
					break
				}
			}
			if ok && dir != "" {
				out[i] = filepath.Join(absRoot, dir)
			} else if mode == GroupFolder && !ok {
				// folder 模式下根目录散图汇成「未分类」，锚点就是根目录
				allRoot := true
				for _, p := range g.Photos {
					if topLevelDir(p.Rel) != "" {
						allRoot = false
						break
					}
				}
				if allRoot {
					out[i] = absRoot
				}
			}
		}
	}
	return out
}

// relOrAbs 尽量以相对路径显示，便于阅读。
func relOrAbs(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}
