// 服务端缩略图生成原语（0.2.1，供 GET /api/v1/thumb 使用）。
//
// 目标：客户端网格浏览目录**不再整文件拉取**（旧路径经 /preview 全量下载
// 再本地缩放）——本模块在服务端把常见光栅图/相机 RAW 内嵌 JPEG 预览缩放为
// ≤w px 的 JPEG 直接回传。
//
// 只用标准库（不引入 golang.org/x/image：build-env 离线镜像无该模块 zip，
// 且新依赖需 go.mod/go.sum 变更——缩放用"迭代 2× 盒式减半 + 末端双线性"
// 自实现，512px 目标下画质足够）。
package api

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 注册 gif 解码（单帧）
	"image/jpeg"
	_ "image/png" // 注册 png 解码
)

// 解码闸门：超限一律按不支持回退（客户端图标/旧路径兜底），防解码炸弹占满
// 内存/CPU——12MP ≈ RGBA 48MB/请求 × 并发槽 ≤4 ≈ 200MB 峰值上界。
const (
	thumbMaxFileBytes = 64 << 20 // 输入文件字节上限（413 判定同值）
	thumbMaxPixels    = 12_000_000
	thumbMaxSideIn    = 6000
	thumbQuality      = 80
)

var errThumbUnsupported = errors.New("thumbnail: unsupported image")

// isRawExt 相机 RAW 扩展名表——与客户端 thumb.rs is_raw_ext、
// preview.ts RAW_EXT 三处同表（全小写、不含点）。
func isRawExt(ext string) bool {
	switch ext {
	case "nef", "nrw", // Nikon
		"cr2", "cr3", "crw", // Canon
		"arw", "srf", "sr2", // Sony
		"raf",        // Fuji
		"rw2", "raw", // Panasonic
		"orf",        // Olympus
		"pef",        // Pentax
		"rwl",        // Leica
		"3fr",        // Hasselblad
		"srw",        // Samsung
		"x3f",        // Sigma
		"erf", "fff", // Epson / Phase One
		"mrw", "dcs", "kdc": // Minolta / Kodak
		return true
	}
	return false
}

// ---------- RAW 内嵌 JPEG 提取（逐字节语义移植客户端 thumb.rs::extract_preview） ----------

func jpegBE16(buf []byte, i int) (int, bool) {
	if i+1 >= len(buf) {
		return 0, false
	}
	return int(buf[i])<<8 | int(buf[i+1]), true
}

// jpegFullLen 若 buf[start..] 是完整 JPEG（FFD8FF 起），返回总长（含 EOI FFD9）。
// 段链前进：带长度头的段按长度跳；SOS 后进入熵数据扫描——FF00 填充/TEM/RSTn
// 合法逐字节前进，progressive 扫描间的表段（DHT 等）终结当前扫描；段边界处
// 非 0xFF 或长度非法即判垃圾候选。只保证"结构完整"，解码侧二次把关。
func jpegFullLen(buf []byte, start int) (int, bool) {
	if buf[start] != 0xFF || buf[start+1] != 0xD8 || buf[start+2] != 0xFF {
		return 0, false
	}
	p := start + 2
	inScan := false
	for {
		if p >= len(buf) {
			return 0, false
		}
		if buf[p] != 0xFF {
			if inScan {
				p++
				continue
			}
			return 0, false
		}
		if p+1 >= len(buf) {
			return 0, false
		}
		m := buf[p+1]
		switch {
		case m == 0xD9: // EOI
			return p + 2 - start, true
		case m == 0xFF: // 填充
			p++
		case m == 0x00 || m == 0x01 || (m >= 0xD0 && m <= 0xD7): // FF00 / TEM / RSTn
			p += 2
		case m == 0xDA: // SOS：跳过头部进入熵数据
			l, ok := jpegBE16(buf, p+2)
			if !ok || l < 2 {
				return 0, false
			}
			p += 2 + l
			inScan = true
		default: // 其余带长度头的段（SOF/DHT/DAC/APPn/COM…）
			l, ok := jpegBE16(buf, p+2)
			if !ok || l < 2 {
				return 0, false
			}
			p += 2 + l
			inScan = false
		}
	}
}

// extractEmbeddedJPEG 扫描缓冲提取所有结构完整的内嵌 JPEG，返回**最大**的
// （相机 RAW 的全尺寸预览；≥128B 拒收碎块）。确无 → nil。
func extractEmbeddedJPEG(buf []byte) []byte {
	bestS, bestL := -1, 0
	i := 0
	for i+3 < len(buf) {
		if buf[i] == 0xFF && buf[i+1] == 0xD8 && buf[i+2] == 0xFF {
			if l, ok := jpegFullLen(buf, i); ok {
				if l >= 128 && l > bestL {
					bestS, bestL = i, l
				}
				if l > 0 {
					i += l // 命中的完整 JPEG 不再进入其内部找嵌套
					continue
				}
			}
		}
		i++
	}
	if bestS < 0 {
		return nil
	}
	return buf[bestS : bestS+bestL]
}

// ---------- 解码 + 缩放 + JPEG 编码 ----------

// decodeScaleJPEG data → ≤maxSide px 长边的 JPEG 字节。闸门：DecodeConfig
// 尺寸超限或解码失败 → errThumbUnsupported。
func decodeScaleJPEG(data []byte, maxSide int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errThumbUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width*cfg.Height > thumbMaxPixels ||
		cfg.Width > thumbMaxSideIn || cfg.Height > thumbMaxSideIn {
		return nil, errThumbUnsupported
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errThumbUnsupported
	}
	small := scaleToFit(img, maxSide)
	out := flattenJPEG(small) // 透明合成白底 + RGBA 规范化
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, out, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, err
	}
	return enc.Bytes(), nil
}

// scaleToFit 长边缩到 ≤maxSide：先迭代 2× 盒式减半（每步质量高、成本 O(n)），
// 直到尺寸落入 2× 目标内，最后一步双线性精调到精确目标尺寸。
func scaleToFit(src image.Image, maxSide int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	tw, th := maxSide, maxSide
	if w >= h {
		th = fitSide(h, w, maxSide)
	} else {
		tw = fitSide(w, h, maxSide)
	}
	if w <= tw && h <= th {
		return src // 已够小，不放大
	}
	cur := toRGBA(src)
	for cur.Bounds().Dx() > tw*2 || cur.Bounds().Dy() > th*2 {
		nw, nh := cur.Bounds().Dx()/2, cur.Bounds().Dy()/2
		if nw < tw {
			nw = tw
		}
		if nh < th {
			nh = th
		}
		if nw == cur.Bounds().Dx() && nh == cur.Bounds().Dy() {
			break
		}
		cur = boxResize(cur, nw, nh)
	}
	return boxResize(cur, tw, th)
}

func fitSide(a, b, maxSide int) int { // a/b 比例下 b→maxSide 时 a 的取整值
	v := (a*maxSide + b/2) / b
	if v < 1 {
		v = 1
	}
	return v
}

func toRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	if rgba, ok := src.(*image.RGBA); ok && b.Min.X == 0 && b.Min.Y == 0 {
		return rgba
	}
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// boxResize 面积平均缩放（每目标像素对源覆盖区做整块均值，2× 减半时即盒式
// 去块，其余比例带权重覆盖，无新依赖）。
func boxResize(src *image.RGBA, nw, nh int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if nw <= 0 || nh <= 0 || sw <= 0 || sh <= 0 {
		return dst
	}
	// 16.16 定点像素映射：每个目标像素覆盖源区 [f0,f1)，边缘部分像素按面积
	// 计权（2× 减半时全部整权 = 盒式去块；末次精调 = 加权面积平均）。
	sxStep := (sw << 16) / nw
	syStep := (sh << 16) / nh
	// 行覆盖区间先行预算（与列无关）
	type rng struct {
		s0, s1 int
		w0, w1 float64
	}
	rows := make([]rng, nh)
	for y := 0; y < nh; y++ {
		f0 := y * syStep
		f1 := f0 + syStep
		if f1 > sh<<16 {
			f1 = sh << 16
		}
		s0, s1 := f0>>16, (f1-1)>>16
		w0 := float64((s0+1)<<16-f0) / 65536.0
		w1 := float64(f1-s1<<16) / 65536.0
		if s1 == s0 {
			w0 += w1
			w1 = 0
		}
		rows[y] = rng{s0, s1, w0, w1}
	}
	for y := 0; y < nh; y++ {
		r := rows[y]
		for x := 0; x < nw; x++ {
			fx0 := x * sxStep
			fx1 := fx0 + sxStep
			if fx1 > sw<<16 {
				fx1 = sw << 16
			}
			sx0, sx1 := fx0>>16, (fx1-1)>>16
			wx0 := float64((sx0+1)<<16-fx0) / 65536.0
			wx1 := float64(fx1-sx1<<16) / 65536.0
			if sx1 == sx0 {
				wx0 += wx1
				wx1 = 0
			}
			var sumR, sumG, sumB, sumA, wsum float64
			for sy := r.s0; sy <= r.s1; sy++ {
				wy := 1.0
				switch sy {
				case r.s0:
					wy = r.w0
				case r.s1:
					wy = r.w1
				}
				// s0==s1 时 w1 已并入 w0（wy 命中 s0 分支即全权）
				rowOff := sb.Min.Y + sy
				for sx := sx0; sx <= sx1; sx++ {
					wx := 1.0
					switch sx {
					case sx0:
						wx = wx0
					case sx1:
						wx = wx1
					}
					wgt := wx * wy
					if wgt <= 0 {
						continue
					}
					i := src.PixOffset(sb.Min.X+sx, rowOff)
					p := src.Pix[i : i+4 : i+4]
					sumR += float64(p[0]) * wgt
					sumG += float64(p[1]) * wgt
					sumB += float64(p[2]) * wgt
					sumA += float64(p[3]) * wgt
					wsum += wgt
				}
			}
			if wsum <= 0 {
				wsum = 1
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o+0] = clamp8(sumR / wsum)
			dst.Pix[o+1] = clamp8(sumG / wsum)
			dst.Pix[o+2] = clamp8(sumB / wsum)
			dst.Pix[o+3] = clamp8(sumA / wsum)
		}
	}
	return dst
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// flattenJPEG 把可能含透明的 RGBA 合成到白底（JPEG 无 alpha；WebView 对
// 透明 PNG 的缩略图预期即白/浅底观感一致）。
func flattenJPEG(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	drawFillWhite(dst)
	rgba := toRGBA(src)
	// 手动 Over 合成（dst 尺寸可能与 rgba 不同——toRGBA 已归零原点）
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			di := dst.PixOffset(x, y)
			si := rgba.PixOffset(x, y)
			a := rgba.Pix[si+3]
			if a == 255 {
				dst.Pix[di+0] = rgba.Pix[si+0]
				dst.Pix[di+1] = rgba.Pix[si+1]
				dst.Pix[di+2] = rgba.Pix[si+2]
				dst.Pix[di+3] = 255
				continue
			}
			fa := float64(a) / 255.0
			for c := 0; c < 3; c++ {
				v := float64(rgba.Pix[si+c])*fa + float64(dst.Pix[di+c])*(1-fa)
				dst.Pix[di+c] = clamp8(v)
			}
			dst.Pix[di+3] = 255
		}
	}
	return dst
}

func drawFillWhite(dst *image.RGBA) {
	white := color.RGBA{255, 255, 255, 255}
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			i := dst.PixOffset(x, y)
			dst.Pix[i+0], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = white.R, white.G, white.B, white.A
		}
	}
}
