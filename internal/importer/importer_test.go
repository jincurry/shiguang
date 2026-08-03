package importer

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exifJPEG 生成带 DateTimeOriginal 的 JPEG。
// TIFF 布局：IFD0@8（1 项指向 Exif IFD@26），Exif IFD@26（1 项，值在 @44）。
func exifJPEG(t *testing.T, dt string) []byte {
	t.Helper()
	var tif bytes.Buffer
	tif.WriteString("II")
	binary.Write(&tif, binary.LittleEndian, uint16(42))
	binary.Write(&tif, binary.LittleEndian, uint32(8))
	// IFD0
	binary.Write(&tif, binary.LittleEndian, uint16(1))
	binary.Write(&tif, binary.LittleEndian, uint16(0x8769)) // ExifIFDPointer
	binary.Write(&tif, binary.LittleEndian, uint16(4))      // LONG
	binary.Write(&tif, binary.LittleEndian, uint32(1))
	binary.Write(&tif, binary.LittleEndian, uint32(26))
	binary.Write(&tif, binary.LittleEndian, uint32(0))
	// Exif IFD @26
	binary.Write(&tif, binary.LittleEndian, uint16(1))
	binary.Write(&tif, binary.LittleEndian, uint16(0x9003)) // DateTimeOriginal
	binary.Write(&tif, binary.LittleEndian, uint16(2))      // ASCII
	binary.Write(&tif, binary.LittleEndian, uint32(20))
	binary.Write(&tif, binary.LittleEndian, uint32(44))
	binary.Write(&tif, binary.LittleEndian, uint32(0))
	tif.WriteString(dt)
	tif.WriteByte(0)

	payload := append([]byte("Exif\x00\x00"), tif.Bytes()...)
	segHeader := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}

	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	raw := jb.Bytes()
	out := append([]byte{}, raw[:2]...) // SOI
	out = append(out, segHeader...)
	out = append(out, payload...)
	return append(out, raw[2:]...)
}

func plainJPEG(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = uint8(int(seed) + i%251)
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func write(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanFiltersByMagicNumber(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.jpg", plainJPEG(t, 1))
	write(t, dir, "renamed.jpg", []byte("this is plain text pretending to be a jpeg"))
	write(t, dir, "notes.txt", []byte("just notes"))
	var pb bytes.Buffer
	png.Encode(&pb, image.NewGray(image.Rect(0, 0, 4, 4)))
	write(t, dir, "shot.png", pb.Bytes())

	photos, skipped, err := Scan(dir, 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 2 {
		t.Fatalf("want 2 importable photos, got %d: %+v", len(photos), photos)
	}
	// 改名的文本要被报出（有图片扩展名），.txt 静默跳过
	if len(skipped) != 1 {
		t.Errorf("want 1 reported skip, got %v", skipped)
	}
	exts := map[string]bool{}
	for _, p := range photos {
		exts[p.Ext] = true
	}
	if !exts["jpg"] || !exts["png"] {
		t.Errorf("expected jpg+png, got %v", exts)
	}
}

func TestScanSkipsOversizeAndHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ok.jpg", plainJPEG(t, 2))
	write(t, dir, "big.jpg", append(plainJPEG(t, 3), bytes.Repeat([]byte{0}, 4096)...))
	write(t, dir, ".thumbnails/hidden.jpg", plainJPEG(t, 4))
	write(t, dir, "@eaDir/nas.jpg", plainJPEG(t, 5))

	photos, skipped, err := Scan(dir, 2048) // 2KB 上限
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range photos {
		if p.Name == "hidden.jpg" || p.Name == "nas.jpg" {
			t.Errorf("should have skipped system dir file: %s", p.Rel)
		}
		if p.Size > 2048 {
			t.Errorf("oversize file leaked through: %s (%d)", p.Rel, p.Size)
		}
	}
	found := false
	for _, s := range skipped {
		if len(s) > 0 && bytes.Contains([]byte(s), []byte("big.jpg")) {
			found = true
		}
	}
	if !found {
		t.Errorf("oversize file should be reported, got %v", skipped)
	}
}

func TestScanReadsEXIFDate(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "withexif.jpg", exifJPEG(t, "2023:04:15 09:30:00"))
	write(t, dir, "noexif.jpg", plainJPEG(t, 7))

	photos, _, err := Scan(dir, 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*Photo{}
	for _, p := range photos {
		byName[p.Name] = p
	}
	got := byName["withexif.jpg"]
	if got == nil || !got.HasEXIF {
		t.Fatalf("EXIF photo not detected: %+v", got)
	}
	if d := got.TakenAt.Format("2006-01-02"); d != "2023-04-15" {
		t.Errorf("EXIF date = %s, want 2023-04-15", d)
	}
	// 无 EXIF 的退回文件修改时间，不应标记 HasEXIF
	if p := byName["noexif.jpg"]; p == nil || p.HasEXIF {
		t.Errorf("non-EXIF photo mis-flagged: %+v", p)
	} else if p.TakenAt.IsZero() {
		t.Error("non-EXIF photo should fall back to mtime")
	}
}

// mkPhoto 构造一张用于分组测试的照片。
func mkPhoto(rel string, taken string) *Photo {
	t, _ := time.Parse("2006-01-02 15:04", taken)
	return &Photo{Rel: rel, Name: filepath.Base(rel), TakenAt: t, Ext: "jpg"}
}

func TestPlanGroupAuto(t *testing.T) {
	photos := []*Photo{
		mkPhoto("黄山/a.jpg", "2024-05-01 10:00"),
		mkPhoto("黄山/b.jpg", "2024-05-02 10:00"),
		mkPhoto("loose1.jpg", "2023-01-05 08:00"),
		mkPhoto("loose2.jpg", "2023-01-05 20:00"),
		mkPhoto("loose3.jpg", "2023-03-09 08:00"),
	}
	groups := Plan(photos, GroupAuto, "批量导入")
	if len(groups) != 3 {
		t.Fatalf("want 3 groups, got %d: %+v", len(groups), groups)
	}
	// 节点按日期倒序
	if groups[0].Date != "2024-05-01" {
		t.Errorf("groups not sorted date-desc: %s first", groups[0].Date)
	}
	byTitle := map[string]*Group{}
	for _, g := range groups {
		byTitle[g.Title] = g
	}
	folder := byTitle["黄山"]
	if folder == nil || len(folder.Photos) != 2 {
		t.Fatalf("folder group wrong: %+v", folder)
	}
	// 组日期取组内最早
	if folder.Date != "2024-05-01" {
		t.Errorf("folder group date = %s, want earliest 2024-05-01", folder.Date)
	}
	if g := byTitle["2023-01-05"]; g == nil || len(g.Photos) != 2 {
		t.Errorf("loose photos should group by date: %+v", g)
	}
	if g := byTitle["2023-03-09"]; g == nil || len(g.Photos) != 1 {
		t.Errorf("second date group wrong: %+v", g)
	}
}

func TestPlanGroupModes(t *testing.T) {
	photos := []*Photo{
		mkPhoto("旅行/a.jpg", "2024-05-01 10:00"),
		mkPhoto("root.jpg", "2023-01-05 08:00"),
	}
	cases := []struct {
		mode      GroupMode
		wantCount int
		wantTitle []string
	}{
		{GroupSingle, 1, []string{"批量导入"}},
		{GroupDate, 2, []string{"2024-05-01", "2023-01-05"}},
		{GroupFolder, 2, []string{"旅行", "未分类"}},
	}
	for _, c := range cases {
		groups := Plan(photos, c.mode, "批量导入")
		if len(groups) != c.wantCount {
			t.Errorf("%s: want %d groups, got %d", c.mode, c.wantCount, len(groups))
			continue
		}
		titles := map[string]bool{}
		for _, g := range groups {
			titles[g.Title] = true
		}
		for _, want := range c.wantTitle {
			if !titles[want] {
				t.Errorf("%s: missing group %q, got %v", c.mode, want, titles)
			}
		}
	}
}

// TestPlanPhotoOrderStable 组内按拍摄时间升序，保证节点内顺序符合直觉且可重现。
func TestPlanPhotoOrderStable(t *testing.T) {
	photos := []*Photo{
		mkPhoto("t/c.jpg", "2024-05-01 18:00"),
		mkPhoto("t/a.jpg", "2024-05-01 08:00"),
		mkPhoto("t/b.jpg", "2024-05-01 12:00"),
	}
	g := Plan(photos, GroupFolder, "")[0]
	want := []string{"t/a.jpg", "t/b.jpg", "t/c.jpg"}
	for i, p := range g.Photos {
		if p.Rel != want[i] {
			t.Errorf("position %d: got %s want %s", i, p.Rel, want[i])
		}
	}
}

func TestParseGroupMode(t *testing.T) {
	for _, ok := range []string{"auto", "date", "folder", "single"} {
		if _, err := ParseGroupMode(ok); err != nil {
			t.Errorf("%s should be valid: %v", ok, err)
		}
	}
	if _, err := ParseGroupMode("nonsense"); err == nil {
		t.Error("invalid mode should error")
	}
}

func TestScanRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.jpg", plainJPEG(t, 9))
	if _, _, err := Scan(p, 30<<20); err == nil {
		t.Error("scanning a file should error")
	}
	if _, _, err := Scan(filepath.Join(dir, "nope"), 30<<20); err == nil {
		t.Error("scanning a missing dir should error")
	}
}

// TestPlanDateFromEXIF 节点日期的可信度由「决定日期的那张照片」决定，
// 而不是组内是否人人都有 EXIF——警告要精确指向真正不可信的节点。
func TestPlanDateFromEXIF(t *testing.T) {
	withEXIF := func(rel, taken string) *Photo {
		p := mkPhoto(rel, taken)
		p.HasEXIF = true
		return p
	}

	// 组内最早那张有 EXIF → 日期可信，尽管另一张没有
	mixed := Plan([]*Photo{
		withEXIF("旅行/a.jpg", "2023-04-15 09:00"),
		mkPhoto("旅行/b.jpg", "2026-08-03 10:00"),
	}, GroupFolder, "")
	if len(mixed) != 1 || !mixed[0].DateFromEXIF {
		t.Errorf("earliest photo has EXIF → date should be trusted: %+v", mixed[0])
	}
	if mixed[0].Date != "2023-04-15" {
		t.Errorf("date = %s, want 2023-04-15", mixed[0].Date)
	}
	if n := mixed[0].EXIFCount(); n != 1 {
		t.Errorf("EXIFCount = %d, want 1", n)
	}

	// 最早那张没 EXIF → 日期不可信，即使组里有别的照片带 EXIF
	untrusted := Plan([]*Photo{
		mkPhoto("旅行/a.jpg", "2020-01-01 09:00"),
		withEXIF("旅行/b.jpg", "2023-04-15 10:00"),
	}, GroupFolder, "")
	if untrusted[0].DateFromEXIF {
		t.Errorf("earliest photo lacks EXIF → date should be flagged: %+v", untrusted[0])
	}

	// 全无 EXIF → 不可信
	none := Plan([]*Photo{mkPhoto("x/a.jpg", "2024-01-01 09:00")}, GroupFolder, "")
	if none[0].DateFromEXIF {
		t.Error("no EXIF at all → date should be flagged")
	}
}
