// Package imgproc 提供图片校验与处理管线（纯函数）以及通用 worker 池。
// 依赖方向：service → imgproc；本包不接触 store 与 blob。
package imgproc

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"time"

	"github.com/buckket/go-blurhash"
	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
	"github.com/rwcarlsen/goexif/exif"

	_ "image/jpeg"
	_ "image/png"
)

// 变体宽度约定（与 blob key 中的 thumb/md/lg 一一对应）。
var VariantWidths = map[string]int{"thumb": 336, "md": 1200, "lg": 2048}

// VariantNames 按尺寸从小到大列出变体名。
var VariantNames = []string{"thumb", "md", "lg"}

var (
	// ErrUnsupported 表示魔数不是 jpeg/png/webp。
	ErrUnsupported = errors.New("imgproc: unsupported media type")
	// ErrTooManyPixels 表示尺寸头超过像素上限（pixel bomb）。
	ErrTooManyPixels = errors.New("imgproc: pixel bomb")
	// ErrCorrupt 表示解码失败（截断/伪造文件）。
	ErrCorrupt = errors.New("imgproc: corrupt image")
)

// SniffFormat 用魔数嗅探格式，仅接受 jpeg/png/webp；返回规范扩展名（jpg/png/webp）。
func SniffFormat(head []byte) (ext string, err error) {
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "jpg", nil
	case len(head) >= 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "png", nil
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp", nil
	}
	return "", ErrUnsupported
}

// ContentTypeForExt 返回扩展名对应的 MIME 类型。
func ContentTypeForExt(ext string) string {
	switch ext {
	case "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

// ExtForContentType 返回 MIME 类型对应的规范扩展名；不支持则返回空串。
func ExtForContentType(ct string) string {
	switch ct {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	}
	return ""
}

// Result 是处理管线的输出。
type Result struct {
	Width    int
	Height   int
	BlurHash string
	Dominant string // #RRGGBB
	TakenAt  *time.Time
	// Variants 是各变体的 webp 字节（key: thumb/md/lg）。
	Variants map[string][]byte
}

// Process 执行完整处理管线：
// 魔数校验 → DecodeConfig 像素上限预检 → 解码 → EXIF Orientation 矫正 →
// EXIF DateTimeOriginal 提取 → 生成 webp 变体（重编码天然剥离全部 EXIF 含 GPS）→
// BlurHash 4×3 + 主色。maxPixels 单位为像素（如 60MP 传 60_000_000）。
func Process(raw []byte, maxPixels int64) (*Result, error) {
	if len(raw) < 12 {
		return nil, ErrCorrupt
	}
	ext, err := SniffFormat(raw[:12])
	if err != nil {
		return nil, err
	}

	// 预读尺寸头：先于全量解码拦截 pixel bomb。
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: decode config: %v", ErrCorrupt, err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("%w: %dx%d", ErrTooManyPixels, cfg.Width, cfg.Height)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}

	res := &Result{Variants: map[string][]byte{}}

	// EXIF 只存在于 JPEG；Orientation 矫正 + 拍摄时间提取。
	if ext == "jpg" {
		if x, err := exif.Decode(bytes.NewReader(raw)); err == nil {
			img = fixOrientation(img, x)
			if dt, err := x.DateTime(); err == nil {
				utc := dt.UTC()
				res.TakenAt = &utc
			}
		}
	}

	b := img.Bounds()
	res.Width, res.Height = b.Dx(), b.Dy()

	for name, w := range VariantWidths {
		target := img
		if b.Dx() > w { // 小图不放大
			target = imaging.Resize(img, w, 0, imaging.Lanczos)
		}
		var buf bytes.Buffer
		if err := webp.Encode(&buf, target, webp.Options{Quality: 80}); err != nil {
			return nil, fmt.Errorf("imgproc: encode %s: %w", name, err)
		}
		res.Variants[name] = buf.Bytes()
	}

	// BlurHash 在小图上计算（性能），4×3 分量。
	small := imaging.Resize(img, 32, 0, imaging.Lanczos)
	bh, err := blurhash.Encode(4, 3, small)
	if err != nil {
		return nil, fmt.Errorf("imgproc: blurhash: %w", err)
	}
	res.BlurHash = bh

	// 主色：缩至 1×1 取均值像素。
	one := imaging.Resize(img, 1, 1, imaging.Lanczos)
	c := color.NRGBAModel.Convert(one.At(0, 0)).(color.NRGBA)
	res.Dominant = fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)

	return res, nil
}

// fixOrientation 按 EXIF Orientation(1-8) 将像素矫正为正立。
func fixOrientation(img image.Image, x *exif.Exif) image.Image {
	tag, err := x.Get(exif.Orientation)
	if err != nil {
		return img
	}
	o, err := tag.Int(0)
	if err != nil {
		return img
	}
	switch o {
	case 2:
		return imaging.FlipH(img)
	case 3:
		return imaging.Rotate180(img)
	case 4:
		return imaging.FlipV(img)
	case 5:
		return imaging.Transpose(img)
	case 6:
		return imaging.Rotate270(img) // 270° CCW = 90° CW
	case 7:
		return imaging.Transverse(img)
	case 8:
		return imaging.Rotate90(img)
	}
	return img
}

// ReadAllLimit 读取 r 全部内容，超过 limit 字节返回错误（防御超大对象回读）。
func ReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("imgproc: object exceeds %d bytes", limit)
	}
	return b, nil
}
