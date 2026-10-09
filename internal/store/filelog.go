// filelog.go：文件元数据存储的二进制日志引擎（0.1.1）。
//
// 目录布局（默认 <dataDir>/filestore，db.dsn 可覆盖）：
//
//	LOCK                 flock 排他锁（单实例防护，0600 空文件）
//	MANIFEST             清单：快照 + 段文件列表（帧容器，tmp+fsync+rename 原子换；
//	MANIFEST.old         上一代清单，崩溃兜底回退）
//	snapshot-NNNNNN.log  全量快照（压缩产物，头一条 opMaxID 为自增水位下界）
//	seg-NNNNNN.log       追加式 WAL 段（末段为当前活动段；段达 8MiB 滚动）
//
// 快照与段共享一个单调文件编号（seq，存于 MANIFEST），按编号升序即创建顺序。
//
// 记录帧格式（需求：op + length + data + crc）：
//
//	op     uint8     0x00+表序 = Put（1..14）；0x10+表序 = Del by PK；0x40 = 清单；0x41 = 自增水位
//	length uint32 LE data 字节数（解码硬上限 16MiB 防畸形）
//	data   [length]  Put/清单/水位 = JSON；Del = PK（整型表 8 字节 LE，文本主键表原样字符串）
//	crc    uint32 LE Castagnoli CRC，覆盖 op||length||data
//
// 每个文件以 12 字节头开始："FMFS"(4) + formatVersion u32 LE(=1) + kind u32 LE
// （1=段 2=快照 3=清单容器）。
//
// 恢复语义：MANIFEST → 快照 → 各段顺序回放。仅**末段**尾部撕裂（EOF/坏 CRC 且其后
// 无可解析记录）被容忍：截断到最后一条完好记录并告警；中段/快照 CRC 失败 = 数据损坏，
// Open 硬失败拒绝启动（静默跳过会半应用一批提交记录）。
package store

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	fsMagic     = "FMFS"
	fsFormatV   = uint32(1)
	fkSegment   = uint32(1)
	fkSnapshot  = uint32(2)
	fkManifestK = uint32(3)

	maxFramePayload = 16 << 20       // 单记录上限 16MiB
	fileHeaderSize  = 4 + 4 + 4      // magic + version + kind
	frameOverhead   = 1 + 4 + 4      // op + length + crc
	flushEveryMs    = 50             // 后台刷盘周期（页缓存）
	segRollBytes    = int64(8 << 20) // 段滚动阈值
	compactAppends  = int64(100_000) // 快照以来追加数阈值
	compactTombMin  = int64(10_000)  // 墓碑数下限（配合 墓碑>活行/4 触发压缩）
)

// 记录操作码。表序 1..13 与 copy.go tableNames 顺序一致。
const (
	opManifest = byte(0x40)
	opMaxID    = byte(0x41)
)

func opPut(table byte) byte { return table } // table ∈ 1..14
func opDel(table byte) byte { return 0x10 + table }

// 表序号常量（与 tableNames 对齐；0.2.3 起 tAuthSessions=14，旧帧 1..13 语义不变，
// maxid 水位数组短 JSON 补零兼容）。
const (
	tUsers = iota + 1
	tUserRoots
	tFileMetas
	tAIDescs
	tTrash
	tGroups
	tFolderACLs
	tShareLinks
	tFileVersions
	tSessions
	tUploadSessions
	tServerMeta
	tAuditEvents
	tAuthSessions
	tCount = tAuthSessions
)

var crcCastagnoli = crc32.MakeTable(crc32.Castagnoli)

var (
	errTornTail = errors.New("filestore: torn record at tail")
	errCorrupt  = errors.New("filestore: corrupt record (crc/length mismatch)")
)

// fsManifest MANIFEST 载荷（JSON）。
type fsManifest struct {
	Snapshot string   `json:"snapshot"`
	Segments []string `json:"segments"`
	Seq      uint64   `json:"seq"` // 下一个可用文件编号
	Closed   bool     `json:"closed,omitempty"`
}

func fsFileName(kind string, seq uint64) string {
	return fmt.Sprintf("%s-%06d.log", kind, seq)
}

// encodeFrame op+len+data+crc → 完整帧字节。
func encodeFrame(op byte, payload []byte) []byte {
	buf := make([]byte, 0, frameOverhead+len(payload))
	buf = append(buf, op)
	lb := make([]byte, 4)
	binary.LittleEndian.PutUint32(lb, uint32(len(payload)))
	buf = append(buf, lb...)
	buf = append(buf, payload...)
	crc := make([]byte, 4)
	binary.LittleEndian.PutUint32(crc, crc32.Checksum(buf, crcCastagnoli))
	return append(buf, crc...)
}

// decodeFrameAt 从 r 读一帧。io.EOF→io.EOF（干净结束）；帧不完整→errTornTail；
// CRC/长度畸形→errCorrupt。
func decodeFrameAt(r *bufio.Reader) (op byte, payload []byte, err error) {
	h := make([]byte, 5)
	if _, err = io.ReadFull(r, h); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, errTornTail
		}
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(h[1:5])
	if n > maxFramePayload {
		return 0, nil, errCorrupt
	}
	buf := make([]byte, int(n)+4)
	if _, err = io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, errTornTail
		}
		return 0, nil, err
	}
	want := binary.LittleEndian.Uint32(buf[n:])
	if crc32.Checksum(append(append([]byte{}, h...), buf[:n]...), crcCastagnoli) != want {
		return 0, nil, errCorrupt
	}
	return h[0], buf[:n], nil
}

func writeFileHeader(w *bufio.Writer, kind uint32) error {
	var hdr [fileHeaderSize]byte
	copy(hdr[:4], fsMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], fsFormatV)
	binary.LittleEndian.PutUint32(hdr[8:12], kind)
	_, err := w.Write(hdr[:])
	return err
}

func readFileHeader(r *bufio.Reader, wantKind uint32) error {
	var hdr [fileHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("filestore: short header: %w", err)
	}
	if string(hdr[:4]) != fsMagic {
		return fmt.Errorf("filestore: bad magic %q", hdr[:4])
	}
	if v := binary.LittleEndian.Uint32(hdr[4:8]); v != fsFormatV {
		return fmt.Errorf("filestore: unsupported format version %d", v)
	}
	if k := binary.LittleEndian.Uint32(hdr[8:12]); k != wantKind {
		return fmt.Errorf("filestore: wrong file kind %d (want %d)", k, wantKind)
	}
	return nil
}

// encodeKey Del 载荷：整型 PK 8 字节 LE；文本 PK 原样。
func encodeKey(id int64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(id))
	return b
}

func decodeIntKey(p []byte) (int64, bool) {
	if len(p) != 8 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(p)), true
}

// ---------- 恢复 ----------

// fsReplay 把 dir 中持久化状态回放进索引；返回各表自增水位（nextID 语义：
// maxSeen+1，含已被墓碑删除的行——id 永不复用）。
func fsReplay(dir string) (*fileIndex, [tCount + 1]int64, fsManifest, error) {
	var nextID [tCount + 1]int64
	idx := newFileIndex()
	man := fsManifest{}

	b, err := readManifestFile(dir)
	if err != nil {
		return nil, nextID, man, err
	}
	if b != nil {
		if err = json.Unmarshal(b, &man); err != nil {
			return nil, nextID, man, fmt.Errorf("filestore: MANIFEST payload: %w", err)
		}
	}
	// 快照
	if man.Snapshot != "" {
		if err = fsReplayFile(filepath.Join(dir, man.Snapshot), fkSnapshot, idx, &nextID, false); err != nil {
			return nil, nextID, man, fmt.Errorf("filestore: snapshot %s: %w", man.Snapshot, err)
		}
	}
	// 段（最后一个允许尾撕裂截断）
	for i, seg := range man.Segments {
		p := filepath.Join(dir, seg)
		isLast := i == len(man.Segments)-1
		if err = fsReplayFile(p, fkSegment, idx, &nextID, isLast); err != nil {
			if errors.Is(err, errTornTail) && isLast {
				log.Printf("filestore: warn: 段尾撕裂，截断 %s（丢弃 %v 尾部字节）", seg, err)
			} else {
				return nil, nextID, man, err
			}
		}
	}
	return idx, nextID, man, nil
}

// fsReplayFile 回放一个文件。isLast 时末帧撕裂→就地截断并返回 errTornTail（供上层
// 降级为告警）；撕裂后若还能解析出完好记录 = 中段损坏，硬错。
func fsReplayFile(path string, kind uint32, idx *fileIndex, nextID *[tCount + 1]int64, isLast bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("filestore: missing file %s", filepath.Base(path))
		}
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err = readFileHeader(r, kind); err != nil {
		return err
	}
	if kind == fkSnapshot { // 快照首条必须为自增水位下界
		op, p, err := decodeFrameAt(r)
		if err != nil {
			return err
		}
		if op != opMaxID {
			return errors.New("filestore: snapshot missing leading maxid record")
		}
		var arr [tCount + 1]int64
		if err = json.Unmarshal(p, &arr); err != nil {
			return fmt.Errorf("filestore: maxid payload: %w", err)
		}
		for i := range nextID {
			if arr[i] > nextID[i] {
				nextID[i] = arr[i]
			}
		}
	}
	// offset = 已消耗字节数（跟踪截断点）
	consumed := int64(fileHeaderSize)
	var pendingErr error
	var pendingAt int64
	for {
		op, p, err := decodeFrameAt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if (errors.Is(err, errTornTail) || errors.Is(err, errCorrupt)) && isLast {
				pendingErr = err
				pendingAt = consumed
				if err2 := drainCheckTail(r); err2 != nil {
					// 尾部之后仍有可解析记录 → 中段损坏
					return fmt.Errorf("%w: %s（损坏记录后仍有已提交数据，拒绝启动）", errCorrupt, filepath.Base(path))
				}
				break // 容忍：截断到 pendingAt
			}
			return fmt.Errorf("%w at %s: %v", errCorrupt, filepath.Base(path), err)
		}
		consumed += int64(frameOverhead + len(p))
		if op == opMaxID {
			var arr [tCount + 1]int64
			if err = json.Unmarshal(p, &arr); err != nil {
				return fmt.Errorf("filestore: maxid payload: %w", err)
			}
			for i := range nextID {
				if arr[i] > nextID[i] {
					nextID[i] = arr[i]
				}
			}
			continue
		}
		if err = idx.applyRec(op, p, nextID); err != nil {
			return fmt.Errorf("filestore: replay %s: %w", filepath.Base(path), err)
		}
	}
	if pendingErr != nil {
		if terr := f.Close(); terr != nil {
			return terr
		}
		if terr := os.Truncate(path, pendingAt); terr != nil {
			return terr
		}
		return pendingErr
	}
	return nil
}

// drainCheckTail 确认撕裂点之后没有完整记录（解析出完整帧 = 中段损坏，返回错误；
// 其余一律视为垃圾尾巴，扫到 EOF 返回 nil）。
func drainCheckTail(r *bufio.Reader) error {
	for {
		_, _, err := decodeFrameAt(r)
		switch {
		case err == nil:
			return errors.New("committed record after corruption")
		case errors.Is(err, io.EOF):
			return nil
		default: // torn/corrupt：继续向后扫描
			if _, derr := r.Peek(1); derr != nil {
				return nil
			}
		}
	}
}

// applyRec 回放一帧 Put/Del。
func (x *fileIndex) applyRec(op byte, payload []byte, nextID *[tCount + 1]int64) error {
	var table byte
	isPut := true
	switch {
	case op >= 0x01 && op <= tCount:
		table = op
	case op >= 0x11 && op <= 0x10+tCount:
		table, isPut = op-0x10, false
	default:
		return fmt.Errorf("filestore: unknown op 0x%02x", op)
	}
	if isPut {
		return x.replayPut(table, payload, nextID)
	}
	return x.replayDel(table, payload)
}

// ---------- 清单读写 ----------

func readManifestFile(dir string) ([]byte, error) {
	for _, name := range []string{"MANIFEST", "MANIFEST.old"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		r := bufio.NewReader(bytes.NewReader(b))
		if err = readFileHeader(r, fkManifestK); err != nil {
			continue
		}
		op, payload, err := decodeFrameAt(r)
		if err != nil || op != opManifest {
			continue
		}
		return payload, nil
	}
	// 无清单：全新存储（或首启）
	bothMissing := true
	for _, name := range []string{"MANIFEST", "MANIFEST.old"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			bothMissing = false
		}
	}
	if bothMissing {
		return nil, nil
	}
	return nil, errors.New("filestore: MANIFEST 存在但不可解析（拒绝以空库覆盖，请人工恢复）")
}

// writeManifestFile 原子写清单（tmp+fsync+代换+dir fsync）。
func writeManifestFile(dir string, man fsManifest) error {
	payload, err := json.Marshal(man)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err = writeFileHeader(w, fkManifestK); err != nil {
		return err
	}
	if _, err = w.Write(encodeFrame(opManifest, payload)); err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "MANIFEST.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// 旧清单转 old 代（崩溃兜底）
	cur := filepath.Join(dir, "MANIFEST")
	if _, err = os.Stat(cur); err == nil {
		if err = os.Rename(cur, filepath.Join(dir, "MANIFEST.old")); err != nil {
			return err
		}
	}
	if err = os.Rename(tmp, cur); err != nil {
		return err
	}
	return fsyncDir(dir)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// listSeq 从文件名取编号（seg/snapshot 通用）。
func fileSeq(name string) uint64 {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || i+6 >= len(name) {
		return 0
	}
	n, err := strconv.ParseUint(name[i+1:i+7], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func sortFileNames(names []string) {
	sort.Slice(names, func(i, j int) bool { return fileSeq(names[i]) < fileSeq(names[j]) })
}
