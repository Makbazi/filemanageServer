package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
)

// handleThumb GET /api/v1/thumb?rootId=&path=&w=（0.2.1，加性端点）。
// 服务端生成 ≤w px 长边的 JPEG 缩略图直接回传——客户端网格浏览目录不再
// 需要把整文件经 /preview 拉进本地缓存（需求："打开服务端文件/文件夹不要
// 触发下载"）。
//
// 守卫逐行对齐 handlePreview：同挂 anon 组、同 degradedView(Download) 判定
// ——匿名语义与 /preview 完全一致（有库匿名 401，需求6 无新增面），字节
// 内容与旧客户端本地缩放所见同源（jpeg/png/gif 直接解码；相机 RAW 取其内嵌
// 最大完整 JPEG 预览，与客户端 thumb.rs 同一算法同扩展表）。
//
// 0.2.2：白名单补 **bmp**（thumbdec.go 纯标准库自实现常见子集 BI_RGB/
// BI_BITFIELDS/RLE8/RLE4——对齐客户端本地缩放对 BMP 的出图能力；畸形 BMP
// 同样 415 回退）。webp 的 VP8 位流无法在"仅标准库+离线构建+零测试面"约束
// 下安全自研，维持 415。
//
// 0.2.4（配合 Client 0.3.6"浏览零整文件"契约）：415/404/413 一律=客户端图标，
// **不再有整文件下载兜底**——为守住"常见照片仍出服务端小图"，JPEG（含 RAW
// 内嵌预览）改走 YCbCr 平面直接降采样（~1.5B/px、无全幅 RGBA 中间层），
// 像素闸门提至 **50MP/单边 9000px**（48MP 手机主摄全覆盖）；PNG/GIF/BMP
// 维持 RGBA 链路 12MP/6000px；webp/heic/tiff 等无解码器形态维持 415→图标。
//
// 资源闸门：输入 ≤64MiB（413）；解码尺寸按上条分档、白名单格式之外一律 415；
// 解码缩放全程受包级信号量限流（并发超限立即 429 不排队，宁让客户端回退
// 图标，不拖垮正常业务面）——JPEG 峰值 ≈平面 75MB+输入 64MiB ≈139MB/槽×4 槽，
// PNG/GIF/BMP ≈48MB/槽×4 槽（见 thumbdec.go 闸门注释）。
func (s *Server) handleThumb(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	if !s.degradedView(c, rootID, rel, func(d perm.Decision) bool { return d.Download }, "download(thumb)") {
		return
	}
	_, abs, err := s.resolve(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(c, http.StatusNotFound, errors.New("file not found"))
		return
	}
	if info.Size() > thumbMaxFileBytes {
		writeErr(c, http.StatusRequestEntityTooLarge, errors.New("file too large for thumbnail"))
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(abs), "."))
	isStd := ext == "jpg" || ext == "jpeg" || ext == "png" || ext == "gif" || ext == "bmp"
	isRAW := isRawExt(ext)
	if !isStd && !isRAW {
		writeErr(c, http.StatusUnsupportedMediaType, errors.New("thumbnail: format not supported"))
		return
	}
	maxSide := 512
	if w := c.Request.URL.Query().Get("w"); w != "" {
		if v, cerr := strconv.Atoi(w); cerr == nil {
			switch {
			case v < 64:
				v = 64
			case v > 1024:
				v = 1024
			}
			maxSide = v
		}
	}
	select {
	case thumbSlots <- struct{}{}:
		defer func() { <-thumbSlots }()
	default:
		writeErr(c, http.StatusTooManyRequests, errors.New("thumbnail generator busy"))
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if isRAW {
		jpegData := extractEmbeddedJPEG(data)
		if jpegData == nil {
			writeErr(c, http.StatusUnsupportedMediaType, errors.New("thumbnail: no embedded preview in raw"))
			return
		}
		data = jpegData
	}
	out, err := decodeScaleJPEG(data, maxSide, ext)
	if err != nil {
		code := http.StatusUnsupportedMediaType
		if !errors.Is(err, errThumbUnsupported) {
			code = http.StatusInternalServerError
		}
		writeErr(c, code, err)
		return
	}
	c.Header("Content-Disposition", `inline; filename="`+filepath.Base(abs)+`"`)
	c.Header("Cache-Control", "private, max-age=86400")
	c.Data(http.StatusOK, "image/jpeg", out)
}

// thumbSlots 缩略图生成并发槽（进程级）：解码+缩放是 CPU/内存密集型，
// 限流宁小勿大（0.2.4 峰值上界：JPEG YCbCr 平面路径 ≈139MB/槽、PNG/GIF/BMP
// RGBA 路径 ≈48MB/槽，×4 槽——见 thumbdec.go 闸门注释）。
var thumbSlots = make(chan struct{}, thumbConcurrency())

func thumbConcurrency() int {
	n := runtime.NumCPU() / 2
	if n < 2 {
		n = 2
	}
	if n > 4 {
		n = 4
	}
	return n
}
