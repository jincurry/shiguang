package importer

import (
	"strings"
	"testing"
)

// TestSafeName 文件名收拾：保留中文、去掉路径分隔符、按字符（而非字节）截断。
func TestSafeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"云海翻涌", "云海翻涌"},
		{"a/b:c*d?e\"f<g>h|i", "a_b_c_d_e_f_g_h_i"},
		{"  前后有空格  ", "前后有空格"},
		{"结尾有点.", "结尾有点"},
		{"多  个   空格", "多 个 空格"},
		{"带\n换行\t制表", "带 换行 制表"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SafeName(c.in); got != c.want {
			t.Errorf("SafeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 60 个字符封顶，且不能把汉字截成半个（按 rune 截断）
	long := strings.Repeat("字", 100)
	got := SafeName(long)
	if n := len([]rune(got)); n != 60 {
		t.Errorf("长名应截到 60 个字符，得到 %d", n)
	}
	if !strings.HasPrefix(long, got) {
		t.Errorf("截断结果不是原串前缀：%q", got)
	}
}

// TestUniqueName 同名节点不能互相覆盖。
func TestUniqueName(t *testing.T) {
	used := map[string]bool{}
	want := []string{"2026-01-01 春节", "2026-01-01 春节 (2)", "2026-01-01 春节 (3)"}
	for i, w := range want {
		if got := uniqueName(used, "2026-01-01 春节"); got != w {
			t.Errorf("第 %d 次: got %q, want %q", i+1, got, w)
		}
	}
	// 带扩展名时序号要插在扩展名之前
	u2 := map[string]bool{}
	uniqueName(u2, "01 图.jpg")
	if got := uniqueName(u2, "01 图.jpg"); got != "01 图 (2).jpg" {
		t.Errorf("带扩展名: got %q", got)
	}
}

// TestPhotoBaseName 序号保证顺序；图注自带扩展名时不要重复后缀。
func TestPhotoBaseName(t *testing.T) {
	cases := []struct {
		idx  int
		cap  string
		id   string
		want string
	}{
		{1, "云海翻涌", "X", "01 云海翻涌.jpg"},
		{12, "DSC1000.jpg", "X", "12 DSC1000.jpg"},
		{3, "IMG_0001.PNG", "X", "03 IMG_0001.png"},
		{7, "", "01ABCDEF", "07 01ABCDEF.jpg"},
	}
	for _, c := range cases {
		got := photoBaseName(c.idx, &PhotoDTO{Caption: c.cap, ID: c.id})
		if got != c.want {
			t.Errorf("photoBaseName(%d,%q) = %q, want %q", c.idx, c.cap, got, c.want)
		}
	}
}

// TestNodeManifestRoundTrip 导出的清单必须能被自己的解析器读回来——这是闭环的关键。
func TestNodeManifestRoundTrip(t *testing.T) {
	n := &NodeDTO{
		Date: "2019-10-06", Title: "小妹的婚礼", Place: "三亚",
		Description: "全家去了三亚\n海边办的", // 故意带换行：清单是行式格式
	}
	m := parseString(t, t.TempDir(), nodeManifest(n))
	if len(m.Warnings) > 0 {
		t.Errorf("导出的清单不该产生告警：%v", m.Warnings)
	}
	if m.Date != n.Date || m.Title != n.Title || m.Place != n.Place {
		t.Errorf("回读不一致：date=%q title=%q place=%q", m.Date, m.Title, m.Place)
	}
	if strings.Contains(m.Description, "\n") {
		t.Errorf("描述应被压成一行：%q", m.Description)
	}
	if !strings.Contains(m.Description, "全家去了三亚") {
		t.Errorf("描述内容丢了：%q", m.Description)
	}
}

// TestPhotoText 照片旁边那份文字：没话可说时不生成文件。
func TestPhotoText(t *testing.T) {
	if got := photoText(&PhotoDTO{}); got != "" {
		t.Errorf("空照片不该生成 txt，得到 %q", got)
	}
	taken := "2019-10-06T08:30:00.000Z"
	got := photoText(&PhotoDTO{Caption: "接亲", Note: "那天下雨", TakenAt: &taken})
	for _, want := range []string{"接亲", "2019-10-06", "相纸背面", "那天下雨"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "T08:30") {
		t.Errorf("拍摄时间应只保留日期部分：%s", got)
	}
}
