package stickers

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ─── 随机数 ───────────────────────────────────────────────────────────────────
//
// math/rand 的全局函数在 Go 1.20+ 已自带锁，但显式持有独立 Rand 更可控：
// 表情选取是低频操作，不值得和别的包争抢全局种子状态。

var (
	rndMu = sync.Mutex{}
	rnd   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func randomInt(n int) int {
	if n <= 1 {
		return 0
	}
	rndMu.Lock()
	defer rndMu.Unlock()
	return rnd.Intn(n)
}

// ─── 本地图片写入 ─────────────────────────────────────────────────────────────

// SaveMedia 把一张图片写入 uploads/ 目录，返回库内相对路径与落盘字节数。
//
// 面板上传与「从图床抓取后本地留存」都走这里，统一做三件事：
//  1. 魔数校验：非图片直接拒绝，避免把任意文件塞进表情目录
//  2. 尺寸限制：超过 MaxMediaBytes 拒绝（随身设备的存储与上行带宽都有限）
//  3. 适度压缩：长边超过 512 时按面积平均缩放，JPEG 重编码为 q84
func SaveMedia(data []byte) (string, int, error) {
	if len(data) == 0 {
		return "", 0, fmt.Errorf("%w: 空文件", ErrInvalid)
	}
	if len(data) > MaxMediaBytes {
		return "", 0, fmt.Errorf("%w: 图片超过 %d KB", ErrInvalid, MaxMediaBytes>>10)
	}

	mime := sniffImageMIME(data)
	if mime == "application/octet-stream" {
		return "", 0, fmt.Errorf("%w: 不是可识别的图片（支持 PNG / JPEG / WebP / GIF）", ErrInvalid)
	}

	optimized, outMIME := optimizeImage(data, mime)
	mu.RLock()
	dir := mediaDir
	mu.RUnlock()
	if dir == "" {
		return "", 0, fmt.Errorf("%w: 表情目录不可用（未配置配置目录）", ErrInvalid)
	}

	rel := "uploads/" + newID() + ExtForMIME(outMIME)
	dst := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(dst, optimized, 0o644); err != nil {
		return "", 0, err
	}
	return rel, len(optimized), nil
}

// maxDim 是表情图片长边的上限。
// 512 已经足够在手机与桌面端显示清晰，再大只是白白吃上行带宽。
const maxDim = 512

// jpegQuality 重编码质量。84 在表情这类小图上肉眼几乎无差。
const jpegQuality = 84

// optimizeImage 尝试缩小体积。任何一步失败都退回原图，绝不因为优化失败而丢掉图片。
func optimizeImage(data []byte, mime string) ([]byte, string) {
	// GIF 可能是多帧动图，重编码会丢帧，直接原样保留
	if mime == "image/gif" {
		if g, err := gif.DecodeAll(bytes.NewReader(data)); err == nil && len(g.Image) > 1 {
			return data, mime
		}
	}

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return data, mime
	}

	scaled := img
	scale := 1.0
	if w > maxDim || h > maxDim {
		scale = float64(maxDim) / float64(w)
		if h > w {
			scale = float64(maxDim) / float64(h)
		}
		nw, nh := int(float64(w)*scale), int(float64(h)*scale)
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		scaled = downscale(img, nw, nh)
	}

	var buf bytes.Buffer
	outMIME := mime
	switch {
	case format == "png":
		// PNG 可能有透明通道，转 JPEG 会把透明区压成黑块，保持 PNG
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, scaled); err != nil {
			return data, mime
		}
		outMIME = "image/png"
	case format == "gif":
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, scaled); err != nil {
			return data, mime
		}
		outMIME = "image/png"
	default:
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return data, mime
		}
		outMIME = "image/jpeg"
	}

	// 只在确实变小、或者确实缩小了尺寸时才采用
	if buf.Len() >= len(data) && scale >= 1.0 {
		return data, mime
	}
	return buf.Bytes(), outMIME
}

// downscale 用面积平均做缩放。
//
// 标准库只有 image/draw 的整体绘制，没有高质量缩放原语；引入
// golang.org/x/image 会把 CI 的 Go 版本要求往上抬，不划算。
// 对「长边压到 512」这种纯缩小场景，面积平均的效果和 CatmullRom 肉眼难分。
func downscale(src image.Image, dstW, dstH int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()

	switch src.(type) {
	case *image.YCbCr:
		if yc, ok := src.(*image.YCbCr); ok {
			return downscaleYCbCr(yc, sb, dstW, dstH)
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	for dy := 0; dy < dstH; dy++ {
		y0 := sb.Min.Y + dy*sh/dstH
		y1 := sb.Min.Y + (dy+1)*sh/dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			x0 := sb.Min.X + dx*sw/dstW
			x1 := sb.Min.X + (dx+1)*sw/dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					cr, cg, cb, ca := src.At(x, y).RGBA()
					r += uint64(cr >> 8)
					g += uint64(cg >> 8)
					b += uint64(cb >> 8)
					a += uint64(ca >> 8)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetNRGBA(dx, dy, color.NRGBA{
				R: uint8(r / n), G: uint8(g / n), B: uint8(b / n), A: uint8(a / n),
			})
		}
	}
	return dst
}

// downscaleYCbCr 走快速路径：JPEG 解码结果就是 YCbCr，
// 直接对亮度平面做面积平均，比逐像素 At() 快一个数量级。
//
// 这里刻意只依赖 YStride / CStride / COffset 这几个在 Go 1.21 上就已存在的
// 字段与方法，不使用 1.27 才改签名的 YOffset，避免抬高 CI 的 Go 版本要求。
func downscaleYCbCr(s *image.YCbCr, sb image.Rectangle, dstW, dstH int) image.Image {
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewYCbCr(image.Rect(0, 0, dstW, dstH), s.SubsampleRatio)
	maxX, maxY := sb.Max.X-1, sb.Max.Y-1

	for dy := 0; dy < dstH; dy++ {
		y0 := sb.Min.Y + dy*sh/dstH
		y1 := sb.Min.Y + (dy+1)*sh/dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > sb.Max.Y {
			y1 = sb.Max.Y
		}
		for dx := 0; dx < dstW; dx++ {
			x0 := sb.Min.X + dx*sw/dstW
			x1 := sb.Min.X + (dx+1)*sw/dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > sb.Max.X {
				x1 = sb.Max.X
			}

			var sy, n uint64
			for y := y0; y < y1; y++ {
				row := (y - sb.Min.Y) * s.YStride
				for x := x0; x < x1; x++ {
					sy += uint64(s.Y[row+(x-sb.Min.X)])
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.Y[dy*dst.YStride+dx] = uint8(sy / n)

			// 色度平面是下采样的：取该块中心点的色度值即可，
			// 反复平均会得到几乎一样的结果，却要多算几十次。
			xc := clampInt((x0+x1)/2, sb.Min.X, maxX)
			yc := clampInt((y0+y1)/2, sb.Min.Y, maxY)
			ci := s.COffset(xc, yc)
			di := dst.COffset(dx, dy)
			dst.Cb[di] = s.Cb[ci]
			dst.Cr[di] = s.Cr[ci]
		}
	}
	return dst
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// DecodeImageFromReader 供面板上传路径使用：限制读取量并做一次解码预检。
func DecodeImageFromReader(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxMediaBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: 图片超过 %d KB", ErrInvalid, limit>>10)
	}
	if sniffImageMIME(data) == "application/octet-stream" {
		return nil, fmt.Errorf("%w: 不是可识别的图片", ErrInvalid)
	}
	return data, nil
}
