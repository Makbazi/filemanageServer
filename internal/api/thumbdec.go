// 服务端缩略图生成原语（0.2.1，供 GET /api/v1/thumb 使用；0.2.2 增设 BMP 解码）。
//
// 目标：客户端网格浏览目录**不再整文件拉取**（旧路径经 /preview 全量下载
// 再本地缩放）——本模块在服务端把常见光栅图/相机 RAW 内嵌 JPEG 预览缩放为
// ≤w px 的 JPEG 直接回传。
//
// 只用标准库（不引入 golang.org/x/image：build-env 离线镜像无该模块 zip，
// 且新依赖需 go.mod/go.sum 变更——缩放用"迭代 2× 盒式减半 + 末端双线性"
// 自实现，512px 目标下画质足够）。
//
// 0.2.2：标准库 image 无 BMP 解码器，而客户端本地缩放（Rust image crate）
// 支持 BMP——为对齐"服务端出图能力=本地机制"，此处自实现常见 BMP 子集
// （BI_RGB 1/4/8 位调色板 + 16/24/32 位、BI_BITFIELDS 16/32 位、BI_RLE8/4，
// 上/下向、4 字节行对齐）；JPEG/PNG 内嵌压缩等罕见变体按不支持（415）回退。
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

// 解码闸门：超限一律按不支持回退（客户端图标——0.2.4 起 Client 0.3.6 浏览
// **永不整文件下载兜底**），防解码炸弹占满内存/CPU——
//   - PNG/GIF/BMP：RGBA 全幅解码，12MP ≈ 48MB/请求 × 并发槽 ≤4 ≈ 200MB 峰值上界；
//   - JPEG（含 RAW 内嵌预览）：0.2.4 起走 YCbCr 平面直接降采样（1.5B/px，
//     无全幅 RGBA 中间层），闸门提至 **50MP/9000px**——48MP 手机主摄/常见单反
//     原图也能出服务端小缩略图（几十 KB），浏览不再因像素闸门落图标；
//     峰值上界 = 平面 ≤75MB + 输入文件 ≤64MiB ≈ 139MB/槽 × 4。
const (
	thumbMaxFileBytes  = 64 << 20 // 输入文件字节上限（413 判定同值）
	thumbMaxPixels     = 12_000_000
	thumbMaxSideIn     = 6000
	thumbMaxPixelsJpeg = 50_000_000 // JPEG/RAW 平面路径专用（见上注释）
	thumbMaxSideJpeg   = 9000
	thumbQuality       = 80
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

// decodeScaleJPEG data → ≤maxSide px 长边的 JPEG 字节。ext 为小写扩展名：
// bmp 走内置解码器（标准库无 BMP 解码，0.2.2 对齐客户端本地缩放能力），
// 其余经 image.Decode 注册表。闸门：尺寸超限或解码失败 → errThumbUnsupported。
func decodeScaleJPEG(data []byte, maxSide int, ext string) ([]byte, error) {
	switch ext {
	case "bmp":
		w, h, cerr := bmpDimensions(data)
		if cerr != nil {
			return nil, errThumbUnsupported
		}
		if w <= 0 || h <= 0 || w*h > thumbMaxPixels || w > thumbMaxSideIn || h > thumbMaxSideIn {
			return nil, errThumbUnsupported
		}
		img, err := decodeBMP(data)
		if err != nil || img == nil {
			return nil, errThumbUnsupported
		}
		return encodeThumb(img, maxSide)
	case "jpg", "jpeg":
		return decodeScaleJPEGPlane(data, maxSide)
	default:
		if isRawExt(ext) { // 调用方已把 data 换成内嵌最大完整 JPEG 预览
			return decodeScaleJPEGPlane(data, maxSide)
		}
		// PNG/GIF：RGBA 全幅解码路径，12MP/6000px 闸门（不变）
		cfg, _, cerr := image.DecodeConfig(bytes.NewReader(data))
		if cerr != nil {
			return nil, errThumbUnsupported
		}
		if cfg.Width <= 0 || cfg.Height <= 0 ||
			cfg.Width*cfg.Height > thumbMaxPixels ||
			cfg.Width > thumbMaxSideIn || cfg.Height > thumbMaxSideIn {
			return nil, errThumbUnsupported
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil || img == nil {
			return nil, errThumbUnsupported
		}
		return encodeThumb(img, maxSide)
	}
}

// decodeScaleJPEGPlane JPEG/RAW 内嵌预览专用（0.2.4）：jpeg.Decode 常规输出为
// *image.YCbCr（基线/渐进、4:4:4·4:2:2·4:2:0…）或 *image.Gray（灰度），
// 平面盒式降采样全程 ~1.5B/px、无全幅 RGBA 中间层——50MP/9000px 高闸门
// 覆盖 48MP 手机主摄原图（旧 12MP 闸门把它们整批打回 415→客户端图标）。
// 异常形态（CMYK→RGBA 等）保守按 12MP 处理，超出直接 415（解码已发生的
// 内存代价由 64MiB 输入闸门封顶）。
func decodeScaleJPEGPlane(data []byte, maxSide int) ([]byte, error) {
	cfg, cerr := jpeg.DecodeConfig(bytes.NewReader(data))
	if cerr != nil {
		return nil, errThumbUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width*cfg.Height > thumbMaxPixelsJpeg ||
		cfg.Width > thumbMaxSideJpeg || cfg.Height > thumbMaxSideJpeg {
		return nil, errThumbUnsupported
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || img == nil {
		return nil, errThumbUnsupported
	}
	switch m := img.(type) {
	case *image.YCbCr:
		small, ok := downscaleYCbCr(m, maxSide)
		if !ok {
			return nil, errThumbUnsupported
		}
		return encodeJPEG(small)
	case *image.Gray:
		small, ok := downscaleGray(m, maxSide)
		if !ok {
			return nil, errThumbUnsupported
		}
		return encodeJPEG(small)
	default:
		b := img.Bounds()
		if b.Dx()*b.Dy() > thumbMaxPixels || b.Dx() > thumbMaxSideIn || b.Dy() > thumbMaxSideIn {
			return nil, errThumbUnsupported
		}
		return encodeThumb(img, maxSide)
	}
}

// encodeThumb 通用链路（RGBA 中间层）：scaleToFit 迭代盒式 + 末端双线性，
// 透明合成白底后编码 JPEG。
func encodeThumb(img image.Image, maxSide int) ([]byte, error) {
	small := scaleToFit(img, maxSide)
	out := flattenJPEG(small) // 透明合成白底 + RGBA 规范化
	return encodeJPEG(out)
}

func encodeJPEG(out image.Image) ([]byte, error) {
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, out, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, err
	}
	return enc.Bytes(), nil
}

// downscaleYCbCr YCbCr 平面盒式降采样直出小 RGBA：Y=盒整数均值，Cb/Cr=盒中心
// 采样（子采样平面本就平滑，缩略图无差别）。所有下标由 Rect/Stride 整数映射
// 得出并带越界守卫（畸形平面不 panic）。ok=false=几何非法。
func downscaleYCbCr(src *image.YCbCr, maxSide int) (*image.RGBA, bool) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 || maxSide <= 0 {
		return nil, false
	}
	tw, th := targetSide(sw, sh, maxSide)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for oy := 0; oy < th; oy++ {
		sy0 := b.Min.Y + oy*sh/th
		sy1 := b.Min.Y + (oy+1)*sh/th
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for ox := 0; ox < tw; ox++ {
			sx0 := b.Min.X + ox*sw/tw
			sx1 := b.Min.X + (ox+1)*sw/tw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sum, cnt uint64
			for sy := sy0; sy < sy1 && sy < b.Max.Y; sy++ {
				base := src.YOffset(b.Min.X, sy)
				if base < 0 || base+sw > len(src.Y) {
					continue
				}
				row := src.Y[base : base+sw]
				for sx := sx0; sx < sx1 && sx < b.Max.X; sx++ {
					sum += uint64(row[sx-b.Min.X])
					cnt++
				}
			}
			if cnt == 0 {
				return nil, false
			}
			ci := src.COffset((sx0+sx1)/2, (sy0+sy1)/2)
			var cbv, crv uint8 = 128, 128 // 越界取中性色（保守出图不 panic）
			if ci >= 0 && ci < len(src.Cb) && ci < len(src.Cr) {
				cbv, crv = src.Cb[ci], src.Cr[ci]
			}
			r, g, bl, _ := color.YCbCr{Y: uint8(sum / cnt), Cb: cbv, Cr: crv}.RGBA()
			dst.SetRGBA(ox, oy, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 0xff})
		}
	}
	return dst, true
}

// downscaleGray 灰度 JPEG 同款（无色度平面）。
func downscaleGray(src *image.Gray, maxSide int) (*image.RGBA, bool) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 || maxSide <= 0 {
		return nil, false
	}
	tw, th := targetSide(sw, sh, maxSide)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for oy := 0; oy < th; oy++ {
		sy0 := b.Min.Y + oy*sh/th
		sy1 := b.Min.Y + (oy+1)*sh/th
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for ox := 0; ox < tw; ox++ {
			sx0 := b.Min.X + ox*sw/tw
			sx1 := b.Min.X + (ox+1)*sw/tw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sum, cnt uint64
			for sy := sy0; sy < sy1 && sy < b.Max.Y; sy++ {
				base := sy*src.Stride + b.Min.X
				if base < 0 || base+sw > len(src.Pix) {
					continue
				}
				row := src.Pix[base : base+sw]
				for sx := sx0; sx < sx1 && sx < b.Max.X; sx++ {
					sum += uint64(row[sx-b.Min.X])
					cnt++
				}
			}
			if cnt == 0 {
				return nil, false
			}
			gv := uint8(sum / cnt)
			dst.SetRGBA(ox, oy, color.RGBA{gv, gv, gv, 0xff})
		}
	}
	return dst, true
}

// targetSide 长边缩到 ≤maxSide 的目标尺寸（与 scaleToFit 同一取整规则；
// 已够小则 1:1 输出，绝不放大）。
func targetSide(sw, sh, maxSide int) (int, int) {
	tw, th := maxSide, maxSide
	if sw >= sh {
		th = fitSide(sh, sw, maxSide)
	} else {
		tw = fitSide(sw, sh, maxSide)
	}
	if sw <= tw && sh <= th {
		tw, th = sw, sh
	}
	return tw, th
}

// ---------- BMP 解码（0.2.2，纯标准库自实现） ----------
//
// 支持常见子集：BITMAPCOREHEADER(12)/BITMAPINFOHEADER(40)/BITMAPV4HEADER(108)/
// BITMAPV5HEADER(124)；BI_RGB 1/4/8 位调色板 + 16/24/32 位真彩、BI_BITFIELDS
// 16/32 位（掩码顺序 R/G/B 按规范）、BI_RLE8/BI_RLE4 游程；负高度=自上而下行序；
// 非 RLE 行 4 字节对齐。JPEG/PNG 内嵌压缩、平面色、alpha-mul 等罕见变体一律
// 报错→415（客户端图标回退）；所有切片越界在读取前判长，畸形文件绝不 panic。

type bmpInfo struct {
	w, h        int // 绝对值尺寸
	topDown     bool
	bpp         uint16
	compression uint32
	dataOff     int
	paletteOff  int
	palN        int       // 调色板项数（bpp<=8；0=真彩）
	palStride   int       // 3=CORE 调色板，4=INFO
	masks       [3]uint32 // R/G/B 掩码（BI_RGB 时取既定默认值）
	rowBytes    int       // 非 RLE 单行字节（含对齐填充）
}

func bmpLE32(b []byte, i int) (uint32, bool) {
	if i < 0 || i+4 > len(b) {
		return 0, false
	}
	return uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24, true
}

func bmpLE16(b []byte, i int) (uint16, bool) {
	if i < 0 || i+2 > len(b) {
		return 0, false
	}
	return uint16(b[i]) | uint16(b[i+1])<<8, true
}

// bmpParse 解析文件头/DIB 头/调色板与像素数据起点；任何字段越界即报错。
func bmpParse(data []byte) (*bmpInfo, error) {
	if len(data) < 26 || data[0] != 'B' || data[1] != 'M' {
		return nil, errors.New("bmp: bad signature")
	}
	dibSize, ok := bmpLE32(data, 14)
	if !ok {
		return nil, errors.New("bmp: truncated header")
	}
	inf := &bmpInfo{}
	clrUsed := 0
	switch int(dibSize) {
	case 12: // BITMAPCOREHEADER：16 位宽高/位数，无 compression（恒 BI_RGB）、3 字节调色板
		w16, _ := bmpLE16(data, 18)
		h16, _ := bmpLE16(data, 20)
		bpp, _ := bmpLE16(data, 24)
		inf.w, inf.h, inf.bpp = int(w16), int(h16), bpp
		inf.paletteOff, inf.palStride = 26, 3
	default:
		if dibSize < 40 || int(dibSize) > len(data)-14 {
			return nil, errors.New("bmp: unsupported dib header")
		}
		w32, _ := bmpLE32(data, 18)
		h32, _ := bmpLE32(data, 22)
		inf.w, inf.h = int(int32(w32)), int(int32(h32))
		inf.bpp, _ = bmpLE16(data, 28)
		inf.compression, _ = bmpLE32(data, 30)
		used, _ := bmpLE32(data, 14+36)
		clrUsed = int(used)
		inf.paletteOff, inf.palStride = 14+int(dibSize), 4
		if inf.h < 0 {
			inf.topDown = true
			inf.h = -inf.h
		}
		switch inf.compression {
		case 3, 6: // BI_BITFIELDS / BI_ALPHABITFIELDS：40 字节头时掩码紧跟头后（R/G/B）
			rm, ok1 := bmpLE32(data, 14+40)
			gm, ok2 := bmpLE32(data, 14+44)
			bm, ok3 := bmpLE32(data, 14+48)
			if !ok1 || !ok2 || !ok3 {
				return nil, errors.New("bmp: truncated bitfields masks")
			}
			inf.masks = [3]uint32{rm, gm, bm}
			if dibSize == 40 {
				inf.paletteOff = 14 + 52
			}
		default:
			switch inf.bpp {
			case 16:
				inf.masks = [3]uint32{0x7C00, 0x03E0, 0x001F}
			case 32:
				inf.masks = [3]uint32{0x00FF0000, 0x0000FF00, 0x000000FF}
			}
		}
	}
	if inf.w <= 0 || inf.h <= 0 {
		return nil, errors.New("bmp: zero dimension")
	}
	switch inf.compression {
	case 0, 1, 2, 3, 6:
	default: // BI_JPEG/BI_PNG/BI_DDBIT 等——不解码
		return nil, errors.New("bmp: unsupported compression")
	}
	switch inf.bpp {
	case 1, 4, 8, 16, 24, 32:
	default:
		return nil, errors.New("bmp: unsupported bpp")
	}
	if (inf.compression == 1 || inf.compression == 2) && inf.bpp > 8 {
		return nil, errors.New("bmp: rle with truecolor bpp")
	}
	if inf.bpp <= 8 {
		n := clrUsed
		if n == 0 || n > 1<<inf.bpp {
			n = 1 << inf.bpp
		}
		inf.palN = n
		if inf.paletteOff+inf.palN*inf.palStride > len(data) {
			return nil, errors.New("bmp: palette past end")
		}
	}
	switch inf.bpp {
	case 1:
		inf.rowBytes = (inf.w + 31) / 32 * 4
	case 4:
		inf.rowBytes = (inf.w + 7) / 8 * 4
	case 8:
		inf.rowBytes = (inf.w + 3) / 4 * 4
	case 16:
		inf.rowBytes = (inf.w*2 + 3) / 4 * 4
	case 24:
		inf.rowBytes = (inf.w*3 + 3) / 4 * 4
	case 32:
		inf.rowBytes = inf.w * 4
	}
	// 像素数据起点：信任文件头 offset，异常则按"调色板之后"推算
	off, ok := bmpLE32(data, 10)
	if !ok || int(off) <= 0 || int(off) >= len(data) {
		inf.dataOff = inf.paletteOff + inf.palN*inf.palStride
	} else {
		inf.dataOff = int(off)
	}
	if inf.dataOff >= len(data) {
		return nil, errors.New("bmp: pixel data past end")
	}
	// 首行必须完整（其余行在解码循环逐行判长）；RLE 无行概念
	if inf.compression == 0 || inf.compression == 3 || inf.compression == 6 {
		if inf.dataOff+inf.rowBytes > len(data) {
			return nil, errors.New("bmp: first row past end")
		}
	}
	return inf, nil
}

// bmpDimensions 只取宽高（12MP/6000px 闸门用——避免超限文件全解码）。
func bmpDimensions(data []byte) (int, int, error) {
	inf, err := bmpParse(data)
	if err != nil {
		return 0, 0, err
	}
	return inf.w, inf.h, nil
}

// bmpMaskShift 掩码最低置位位。
func bmpMaskShift(m uint32) uint {
	s := uint(0)
	for m != 0 && m&1 == 0 {
		m >>= 1
		s++
	}
	return s
}

func bmpMaskBits(m uint32) uint {
	n := uint(0)
	for m != 0 {
		if m&1 == 1 {
			n++
		}
		m >>= 1
	}
	return n
}

// bmpSample 按掩码提取通道并位宽精确拉伸到 8 位（val*255/max；5/6/8 位组合）。
func bmpSample(v uint32, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := bmpMaskShift(mask)
	val := (v >> shift) & mask
	bits := bmpMaskBits(mask)
	if bits >= 8 {
		return uint8(val)
	}
	return uint8(val * 255 / uint32(1<<bits-1))
}

func decodeBMP(data []byte) (image.Image, error) {
	inf, err := bmpParse(data)
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(image.Rect(0, 0, inf.w, inf.h))
	setPix := func(x, y int, c color.RGBA) {
		if inf.topDown {
			dst.SetRGBA(x, y, c)
		} else {
			dst.SetRGBA(x, inf.h-1-y, c)
		}
	}
	// 调色板（仅索引色；RLE 亦只有索引色）
	var pal []color.RGBA
	if inf.palN > 0 {
		pal = make([]color.RGBA, inf.palN)
		for i := 0; i < inf.palN; i++ {
			o := inf.paletteOff + i*inf.palStride
			pal[i] = color.RGBA{B: data[o], G: data[o+1], R: data[o+2], A: 255}
		}
	}
	switch inf.compression {
	case 1:
		if err := inf.rleWalk(data, pal, setPix, 8); err != nil {
			return nil, err
		}
		return dst, nil
	case 2:
		if err := inf.rleWalk(data, pal, setPix, 4); err != nil {
			return nil, err
		}
		return dst, nil
	}
	alphaByte := inf.compression == 6 || inf.masks[0]&0x80000000 != 0
	for y := 0; y < inf.h; y++ {
		row := inf.dataOff + y*inf.rowBytes
		if row+inf.rowBytes > len(data) {
			return nil, errors.New("bmp: truncated rows")
		}
		line := data[row : row+inf.rowBytes]
		switch inf.bpp {
		case 1:
			for x := 0; x < inf.w; x++ {
				ix := int(line[x/8] >> (7 - uint(x%8)) & 1)
				setPix(x, y, pal[ix])
			}
		case 4:
			for x := 0; x < inf.w; x++ {
				v := line[x/2]
				if x%2 == 0 {
					v >>= 4
				}
				setPix(x, y, pal[v&0x0F])
			}
		case 8:
			for x := 0; x < inf.w; x++ {
				setPix(x, y, pal[line[x]])
			}
		case 16:
			for x := 0; x < inf.w; x++ {
				v, _ := bmpLE16(line, x*2)
				setPix(x, y, color.RGBA{
					R: bmpSample(uint32(v), inf.masks[0]),
					G: bmpSample(uint32(v), inf.masks[1]),
					B: bmpSample(uint32(v), inf.masks[2]),
					A: 255,
				})
			}
		case 24:
			for x := 0; x < inf.w; x++ {
				o := x * 3
				setPix(x, y, color.RGBA{R: line[o+2], G: line[o+1], B: line[o], A: 255})
			}
		case 32:
			for x := 0; x < inf.w; x++ {
				o := x * 4
				v, _ := bmpLE32(line, o)
				a := uint8(255)
				if alphaByte {
					a = uint8(v >> 24)
				}
				setPix(x, y, color.RGBA{
					R: bmpSample(v, inf.masks[0]),
					G: bmpSample(v, inf.masks[1]),
					B: bmpSample(v, inf.masks[2]),
					A: a,
				})
			}
		}
	}
	return dst, nil
}

// rleWalk BI_RLE8/BI_RLE4 共享游程状态机（bitsPerIndex=8 或 4）。
// 命令：count=0 转义（0 行尾 / 1 文件尾 / 2 位移）；count>0 且 cmd=0 字面 run；
// 否则 (count,索引) 重复 run——RLE4 重复 run 交替 cmd 高低半字节、字面 run 为
// 半字节流；字面 run 后偶数字节对齐。越界一律报错（客户端图标回退）。
func (inf *bmpInfo) rleWalk(data []byte, pal []color.RGBA, setPix func(int, int, color.RGBA), bitsPerIndex int) error {
	x, y := 0, 0
	put := func(c color.RGBA) {
		if x >= 0 && x < inf.w && y >= 0 && y < inf.h {
			setPix(x, y, c)
		}
		x++
	}
	p := inf.dataOff
	for p+1 < len(data) {
		cnt, cmd := data[p], data[p+1]
		p += 2
		if cnt == 0 {
			switch cmd {
			case 0: // EOL
				x, y = 0, y+1
			case 1: // EOF
				return nil
			case 2: // delta
				if p+2 > len(data) {
					return errors.New("bmp: rle delta past end")
				}
				x += int(data[p])
				y += int(data[p+1])
				p += 2
			default:
				return errors.New("bmp: bad rle escape")
			}
			continue
		}
		if cmd == 0 { // 字面 run
			nbytes := int(cnt) * bitsPerIndex / 8
			if int(cnt)*bitsPerIndex%8 != 0 {
				nbytes++
			}
			if p+nbytes > len(data) {
				return errors.New("bmp: rle literal past end")
			}
			for i := 0; i < int(cnt); i++ {
				var v byte
				if bitsPerIndex == 8 {
					v = data[p+i]
				} else if i%2 == 0 {
					v = data[p+i/2] >> 4
				} else {
					v = data[p+i/2] & 0x0F
				}
				if int(v) >= len(pal) {
					return errors.New("bmp: rle index out of palette")
				}
				put(pal[v])
			}
			p += nbytes
			if nbytes%2 == 1 && p < len(data) {
				p++
			}
			continue
		}
		// 重复 run
		hi, lo := cmd>>4, cmd&0x0F
		if bitsPerIndex == 8 {
			if int(cmd) >= len(pal) {
				return errors.New("bmp: rle index out of palette")
			}
			for i := 0; i < int(cnt); i++ {
				put(pal[cmd])
			}
		} else {
			if int(hi) >= len(pal) || int(lo) >= len(pal) {
				return errors.New("bmp: rle index out of palette")
			}
			for i := 0; i < int(cnt); i++ {
				if i%2 == 0 {
					put(pal[hi])
				} else {
					put(pal[lo])
				}
			}
		}
	}
	return nil
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
