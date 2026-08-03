package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseString(t *testing.T, dir, content string) *Manifest {
	t.Helper()
	p := filepath.Join(dir, ManifestName)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseManifest(p, f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseManifest(t *testing.T) {
	m := parseString(t, t.TempDir(), `# 注释行
   # 缩进的注释也算

date = 2019-10-06
title = 小妹的婚礼
description = 全家去了三亚，海边办的

DSC1000.jpg = 接亲那天早上
DSC1001.jpg =
IMG_2.JPG = 大小写文件名
含 = 号的图注.jpg = 值里也有 = 号
没有等号的行
`)
	if m.Date != "2019-10-06" || m.Title != "小妹的婚礼" {
		t.Errorf("node fields: %+v", m)
	}
	if m.Description != "全家去了三亚，海边办的" {
		t.Errorf("description = %q", m.Description)
	}
	if c, _ := m.Caption("DSC1000.jpg"); c != "接亲那天早上" {
		t.Errorf("caption = %q", c)
	}
	// 空值也算显式设置（用于清掉图注）
	if c, ok := m.Caption("DSC1001.jpg"); !ok || c != "" {
		t.Errorf("empty caption should be recorded: %q %v", c, ok)
	}
	// 文件名大小写不敏感
	if c, _ := m.Caption("img_2.jpg"); c != "大小写文件名" {
		t.Errorf("case-insensitive lookup failed: %q", c)
	}
	// 文件名里含「=」会被首个 = 切错 —— 必须明确报出来而不是静默存错值
	if _, ok := m.Caption("含 = 号的图注.jpg"); ok {
		t.Error("filename containing '=' should not be silently accepted")
	}
	// 两条警告：没有 = 的行、被切错的那行
	if len(m.Warnings) != 2 {
		t.Fatalf("warnings = %v", m.Warnings)
	}
	joined := strings.Join(m.Warnings, " | ")
	if !strings.Contains(joined, "缺少") || !strings.Contains(joined, "不像文件名") {
		t.Errorf("warnings should cover both cases: %v", m.Warnings)
	}
}

// TestParseManifestValueKeepsEquals 值里的「=」正常保留（键本身没有歧义时）。
func TestParseManifestValueKeepsEquals(t *testing.T) {
	m := parseString(t, t.TempDir(), "a.jpg = 公式 1+1 = 2 的那张\n")
	if c, _ := m.Caption("a.jpg"); c != "公式 1+1 = 2 的那张" {
		t.Errorf("value should keep '=': %q", c)
	}
	if len(m.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", m.Warnings)
	}
}

func TestParseManifestValidation(t *testing.T) {
	m := parseString(t, t.TempDir(), "date = 2019/10/06\ntitle = "+
		strings.Repeat("长", 130)+"\nx.jpg = "+strings.Repeat("字", 210)+"\n")
	if m.Date != "" {
		t.Errorf("invalid date should be ignored, got %q", m.Date)
	}
	if n := len([]rune(m.Title)); n != 120 {
		t.Errorf("title should clamp to 120, got %d", n)
	}
	c, _ := m.Caption("x.jpg")
	if n := len([]rune(c)); n != 200 {
		t.Errorf("caption should clamp to 200, got %d", n)
	}
	if len(m.Warnings) != 3 {
		t.Errorf("expected 3 warnings, got %v", m.Warnings)
	}
}

func TestParseManifestBOM(t *testing.T) {
	// Windows 记事本存 UTF-8 会带 BOM，不能因此丢掉第一行
	m := parseString(t, t.TempDir(), "\ufefftitle = 有BOM的标题\n")
	if m.Title != "有BOM的标题" {
		t.Errorf("BOM not stripped: %q", m.Title)
	}
}

// TestNoManifestUnchanged 没有清单时，分组结果与不带清单功能时完全一致。
func TestNoManifestUnchanged(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "旅行/a.jpg", plainJPEG(t, 1))
	write(t, dir, "旅行/b.jpg", plainJPEG(t, 2))
	write(t, dir, "root.jpg", plainJPEG(t, 3))

	groups, photos, warnings, err := ScanAndPlan(dir, GroupAuto, "批量导入", 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("no manifest should produce no warnings, got %v", warnings)
	}
	for _, p := range photos {
		if p.Caption != "" {
			t.Errorf("caption should stay empty without manifest: %s → %q", p.Rel, p.Caption)
		}
	}
	for _, g := range groups {
		if g.Description != "" || g.DateFromManifest {
			t.Errorf("group should be untouched: %+v", g)
		}
	}
}

func TestApplyManifestNodeFieldsAndCaptions(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "婚礼/a.jpg", plainJPEG(t, 1))
	write(t, dir, "婚礼/b.jpg", plainJPEG(t, 2))
	write(t, dir, "婚礼/"+ManifestName, []byte(
		"date = 2019-10-06\ntitle = 小妹的婚礼\ndescription = 海边办的\na.jpg = 接亲那天早上\n"))

	groups, photos, warnings, err := ScanAndPlan(dir, GroupAuto, "", 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Date != "2019-10-06" || g.Title != "小妹的婚礼" || g.Description != "海边办的" {
		t.Errorf("node fields not applied: %+v", g)
	}
	if !g.DateFromManifest {
		t.Error("DateFromManifest should be set")
	}
	byName := map[string]*Photo{}
	for _, p := range photos {
		byName[p.Name] = p
	}
	if byName["a.jpg"].Caption != "接亲那天早上" {
		t.Errorf("caption not applied: %q", byName["a.jpg"].Caption)
	}
	// 清单里没写的照片保持空，上传阶段回落到文件名
	if byName["b.jpg"].Caption != "" {
		t.Errorf("unlisted photo should keep empty caption: %q", byName["b.jpg"].Caption)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// TestApplyManifestTypoWarning 清单里写了目录下不存在的文件名要报出来。
func TestApplyManifestTypoWarning(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "旅行/a.jpg", plainJPEG(t, 1))
	write(t, dir, "旅行/"+ManifestName, []byte("拼错了.jpg = 图注\n"))

	_, _, warnings, err := ScanAndPlan(dir, GroupAuto, "", 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "拼错了.jpg") {
		t.Errorf("typo should be reported, got %v", warnings)
	}
}

// TestApplyManifestNodeFieldsIgnoredWhenAmbiguous 目录不唯一对应一个节点时，
// 节点级字段不生效并明确告知；图注仍然生效。
func TestApplyManifestNodeFieldsIgnoredWhenAmbiguous(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "混合/a.jpg", plainJPEG(t, 1))
	write(t, dir, "混合/"+ManifestName, []byte("title = 不该生效\na.jpg = 但图注要生效\n"))

	// date 模式下节点按拍摄日期成组，与目录无关
	groups, photos, warnings, err := ScanAndPlan(dir, GroupDate, "", 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Title == "不该生效" {
			t.Error("node title should not apply in date mode")
		}
	}
	if photos[0].Caption != "但图注要生效" {
		t.Errorf("caption should still apply: %q", photos[0].Caption)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "未生效") {
		t.Errorf("should warn about ignored node fields, got %v", warnings)
	}
}

func TestEmitManifests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "旅行/a.jpg", plainJPEG(t, 1))
	write(t, dir, "旅行/子目录/b.jpg", plainJPEG(t, 2))

	groups, _, _, err := ScanAndPlan(dir, GroupAuto, "", 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	res, err := EmitManifests(dir, groups, GroupAuto)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("want 1 manifest written, got %v", res.Written)
	}
	body, err := os.ReadFile(filepath.Join(dir, "旅行", ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "title = 旅行") {
		t.Errorf("template missing title:\n%s", s)
	}
	// 只列本目录直属照片，子目录的照片归子目录自己的清单
	if !strings.Contains(s, "a.jpg = a") {
		t.Errorf("template missing direct photo:\n%s", s)
	}
	if strings.Contains(s, "b.jpg") {
		t.Errorf("template should not list nested photo:\n%s", s)
	}

	// 再跑一次不能覆盖用户改过的内容
	if err := os.WriteFile(filepath.Join(dir, "旅行", ManifestName),
		[]byte("title = 我改过的\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := EmitManifests(dir, groups, GroupAuto)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Written) != 0 || len(res2.Skipped) != 1 {
		t.Errorf("existing manifest must not be overwritten: %+v", res2)
	}
	body2, _ := os.ReadFile(filepath.Join(dir, "旅行", ManifestName))
	if string(body2) != "title = 我改过的\n" {
		t.Errorf("user content was clobbered: %q", body2)
	}
}

// TestManifestFileNotScannedAsPhoto 清单文件本身不能被当成待导入文件报错。
func TestManifestFileNotScannedAsPhoto(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.jpg", plainJPEG(t, 1))
	write(t, dir, ManifestName, []byte("title = x\n"))

	photos, skipped, err := Scan(dir, 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 1 {
		t.Errorf("want 1 photo, got %d", len(photos))
	}
	for _, s := range skipped {
		if strings.Contains(s, ManifestName) {
			t.Errorf("manifest should not be reported as skipped photo: %s", s)
		}
	}
}
