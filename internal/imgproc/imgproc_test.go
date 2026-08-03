package imgproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
)

const testMaxPixels = 60_000_000

// upright 生成"正立基准图"：左半红、右半蓝，宽 128 高 64（宽 > 高便于检查转置）。
func upright() *image.NRGBA {
	img := imaging.New(128, 64, color.NRGBA{0, 0, 0, 255})
	for y := 0; y < 64; y++ {
		for x := 0; x < 128; x++ {
			c := color.NRGBA{255, 0, 0, 255}
			if x >= 64 {
				c = color.NRGBA{0, 0, 255, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// storedFor 生成按 EXIF orientation o 存储时的像素（即矫正函数的逆变换）。
func storedFor(o int, base image.Image) image.Image {
	switch o {
	case 2:
		return imaging.FlipH(base)
	case 3:
		return imaging.Rotate180(base)
	case 4:
		return imaging.FlipV(base)
	case 5:
		return imaging.Transpose(base)
	case 6:
		return imaging.Rotate90(base) // 矫正用 Rotate270，其逆为 Rotate90
	case 7:
		return imaging.Transverse(base)
	case 8:
		return imaging.Rotate270(base)
	}
	return base
}

// exifApp1 构造只含 Orientation 标签的最小 EXIF APP1 段。
func exifApp1(orientation uint16) []byte {
	var tiff bytes.Buffer
	tiff.WriteString("II")                                  // 小端
	binary.Write(&tiff, binary.LittleEndian, uint16(42))    // TIFF magic
	binary.Write(&tiff, binary.LittleEndian, uint32(8))     // IFD0 偏移
	binary.Write(&tiff, binary.LittleEndian, uint16(1))     // 1 个 entry
	binary.Write(&tiff, binary.LittleEndian, uint16(0x112)) // Orientation
	binary.Write(&tiff, binary.LittleEndian, uint16(3))     // SHORT
	binary.Write(&tiff, binary.LittleEndian, uint32(1))     // count
	binary.Write(&tiff, binary.LittleEndian, orientation)
	binary.Write(&tiff, binary.LittleEndian, uint16(0)) // value 补齐 4 字节
	binary.Write(&tiff, binary.LittleEndian, uint32(0)) // 下一 IFD = 无

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	return append(seg, payload...)
}

// jpegWithOrientation 编码 JPEG 并在 SOI 后插入 EXIF Orientation 段。
func jpegWithOrientation(t *testing.T, img image.Image, o int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	out := append([]byte{}, raw[:2]...) // SOI
	out = append(out, exifApp1(uint16(o))...)
	return append(out, raw[2:]...)
}

// avgColorAt 解码 webp 变体并采样某相对位置的颜色。
func sampleVariant(t *testing.T, data []byte, fx, fy float64) color.NRGBA {
	t.Helper()
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode webp: %v", err)
	}
	b := img.Bounds()
	x := b.Min.X + int(float64(b.Dx())*fx)
	y := b.Min.Y + int(float64(b.Dy())*fy)
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func TestProcessEXIFOrientations(t *testing.T) {
	base := upright()
	for o := 1; o <= 8; o++ {
		stored := storedFor(o, base)
		raw := jpegWithOrientation(t, stored, o)
		res, err := Process(raw, testMaxPixels)
		if err != nil {
			t.Fatalf("orientation %d: %v", o, err)
		}
		// 矫正后的尺寸必须恢复为 128×64
		if res.Width != 128 || res.Height != 64 {
			t.Errorf("orientation %d: got %dx%d, want 128x64", o, res.Width, res.Height)
		}
		// 左侧应是红、右侧应是蓝（允许 JPEG 有损误差）
		left := sampleVariant(t, res.Variants["thumb"], 0.25, 0.5)
		right := sampleVariant(t, res.Variants["thumb"], 0.75, 0.5)
		if left.R < 180 || left.B > 90 {
			t.Errorf("orientation %d: left pixel not red: %+v", o, left)
		}
		if right.B < 180 || right.R > 90 {
			t.Errorf("orientation %d: right pixel not blue: %+v", o, right)
		}
	}
}

func TestProcessGrayscalePNG(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 40, 30))
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 256)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	res, err := Process(buf.Bytes(), testMaxPixels)
	if err != nil {
		t.Fatalf("grayscale png: %v", err)
	}
	if res.Width != 40 || res.Height != 30 {
		t.Errorf("got %dx%d, want 40x30", res.Width, res.Height)
	}
	if res.BlurHash == "" || res.Dominant == "" {
		t.Error("missing blurhash/dominant")
	}
	for _, name := range VariantNames {
		if len(res.Variants[name]) == 0 {
			t.Errorf("variant %s empty", name)
		}
	}
}

func TestProcessTruncated(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, upright(), nil)
	raw := buf.Bytes()[:buf.Len()/2] // 拦腰截断
	_, err := Process(raw, testMaxPixels)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated jpeg: got %v, want ErrCorrupt", err)
	}
}

// TestProcessPixelBomb 用伪造的巨大 IHDR 尺寸头验证 60MP 上限在解码前生效。
func TestProcessPixelBomb(t *testing.T) {
	var ihdr bytes.Buffer
	binary.Write(&ihdr, binary.BigEndian, uint32(60000)) // width
	binary.Write(&ihdr, binary.BigEndian, uint32(60000)) // height
	ihdr.Write([]byte{8, 2, 0, 0, 0})                    // bit depth / RGB / ...

	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	binary.Write(&buf, binary.BigEndian, uint32(ihdr.Len()))
	chunk := append([]byte("IHDR"), ihdr.Bytes()...)
	buf.Write(chunk)
	binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))

	_, err := Process(buf.Bytes(), testMaxPixels)
	if !errors.Is(err, ErrTooManyPixels) {
		t.Errorf("pixel bomb: got %v, want ErrTooManyPixels", err)
	}
}

func TestProcessUnsupported(t *testing.T) {
	_, err := Process([]byte("this is definitely not an image file"), testMaxPixels)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("text file: got %v, want ErrUnsupported", err)
	}
}

func TestSniffFormat(t *testing.T) {
	cases := []struct {
		head []byte
		ext  string
		ok   bool
	}{
		{[]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, "jpg", true},
		{[]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}, "png", true},
		{[]byte("RIFF\x00\x00\x00\x00WEBP"), "webp", true},
		{[]byte("GIF89a______"), "", false},
		{[]byte("<html>______"), "", false},
	}
	for _, c := range cases {
		ext, err := SniffFormat(c.head)
		if c.ok && (err != nil || ext != c.ext) {
			t.Errorf("sniff %q: got (%s,%v), want %s", c.head[:4], ext, err, c.ext)
		}
		if !c.ok && err == nil {
			t.Errorf("sniff %q: expected error", c.head[:4])
		}
	}
}

// TestNoUpscale 小图不应放大。
func TestNoUpscale(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, imaging.New(100, 80, color.NRGBA{10, 200, 30, 255}), nil)
	res, err := Process(buf.Bytes(), testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	img, err := webp.Decode(bytes.NewReader(res.Variants["lg"]))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 100 {
		t.Errorf("lg variant upscaled: %d, want 100", img.Bounds().Dx())
	}
}
