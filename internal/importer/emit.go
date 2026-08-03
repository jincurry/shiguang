package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EmitResult 是生成清单模板的结果。
type EmitResult struct {
	Written []string // 新写入的清单路径（相对扫描根）
	Skipped []string // 已存在、未覆盖的
	NoDir   []*Group // 没有唯一对应目录、无处安放清单的节点
}

// EmitManifests 按当前推导结果，在每个节点对应的目录里生成 shiguang.txt 模板。
// 已存在的清单一律跳过不覆盖——清单是用户手写的内容，不能被工具冲掉。
func EmitManifests(root string, groups []*Group, mode GroupMode) (*EmitResult, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	anchors := groupAnchors(absRoot, groups, mode)
	res := &EmitResult{}

	for i, g := range groups {
		dir := anchors[i]
		if dir == "" {
			res.NoDir = append(res.NoDir, g)
			continue
		}
		path := filepath.Join(dir, ManifestName)
		if _, err := os.Stat(path); err == nil {
			res.Skipped = append(res.Skipped, relOrAbs(absRoot, path))
			continue
		}
		if err := os.WriteFile(path, []byte(renderManifest(g)), 0o644); err != nil {
			return nil, fmt.Errorf("写入 %s: %w", path, err)
		}
		res.Written = append(res.Written, relOrAbs(absRoot, path))
	}
	return res, nil
}

// renderManifest 生成一份填好当前推导值的清单模板。
func renderManifest(g *Group) string {
	var b strings.Builder
	b.WriteString("# 拾光集导入清单 —— 改完保存，重新运行 sgctl import 即可生效\n")
	b.WriteString("# 以 # 开头的行是注释；值写到行尾即可，不需要引号\n")
	b.WriteString("# 删掉某一行 = 该项回到自动推导\n\n")

	b.WriteString("# 节点日期（YYYY-MM-DD）\n")
	if !g.DateFromEXIF {
		b.WriteString("# ⚠ 下面这个日期来自文件修改时间，不是 EXIF 拍摄时间，多半需要改\n")
	}
	fmt.Fprintf(&b, "date = %s\n\n", g.Date)

	b.WriteString("# 节点标题（最多 120 字）\n")
	fmt.Fprintf(&b, "title = %s\n\n", g.Title)

	b.WriteString("# 节点描述（最多 2000 字，单行）\n")
	fmt.Fprintf(&b, "description = %s\n\n", g.Description)

	b.WriteString("# 每张照片的图注（最多 200 字）；留空则用文件名\n")
	for _, p := range g.Photos {
		name := p.Name
		// 只列出本目录直属的照片：子目录里的照片归属另一个目录的清单
		if strings.Contains(strings.TrimPrefix(p.Rel, topLevelDir(p.Rel)+"/"), "/") {
			continue
		}
		caption := p.Caption
		if caption == "" {
			caption = strings.TrimSuffix(name, filepath.Ext(name))
		}
		fmt.Fprintf(&b, "%s = %s\n", name, caption)
	}
	return b.String()
}
