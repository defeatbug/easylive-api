package mediarec

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// vp8Keyframe 造一个结构正确的最小 VP8 关键帧：
// 3 字节帧标签（bit0=0 表示关键帧）+ 起始码 9d 01 2a + 2 字节宽 + 2 字节高 + 载荷。
func vp8Keyframe(w, h uint16, payloadLen int) []byte {
	buf := make([]byte, 10+payloadLen)
	buf[0], buf[1], buf[2] = 0x10, 0x00, 0x00 // 帧标签：keyframe(bit0=0) + 版本 0
	buf[3], buf[4], buf[5] = 0x9d, 0x01, 0x2a // VP8 起始码
	binary.LittleEndian.PutUint16(buf[6:8], w)
	binary.LittleEndian.PutUint16(buf[8:10], h)
	for i := 10; i < len(buf); i++ {
		buf[i] = 0x55
	}
	return buf
}

// vp8Interframe 造一个最小非关键帧（bit0=1）。
func vp8Interframe(payloadLen int) []byte {
	buf := make([]byte, 3+payloadLen)
	buf[0], buf[1], buf[2] = 0x11, 0x00, 0x00
	for i := 3; i < len(buf); i++ {
		buf[i] = 0x33
	}
	return buf
}

// rtpPacket 把 VP8 帧打包成 RTP：payload descriptor(0x10=S=1 表示**帧起始**) + 帧数据。
// 真实抓包证实：帧的起始分片 S=1，续片 S=0（见 mediarec 的离线分析）。
func rtpPacket(frame []byte, ts uint32, marker bool) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(ts), Timestamp: ts, SSRC: 1234, Marker: marker},
		Payload: append([]byte{0x10}, frame...),
	}
}

// rtpContinuation 造一个**续片**（S=0）：它不是帧的起点，单独出现时必须被丢弃。
func rtpContinuation(frame []byte, ts uint32, marker bool) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(ts), Timestamp: ts, SSRC: 1234, Marker: marker},
		Payload: append([]byte{0x00}, frame...),
	}
}

// TestIVFHeaderAndFrames 校验容器结构：魔数、fourcc、时间基、分辨率、帧数与时间戳。
func TestIVFHeaderAndFrames(t *testing.T) {
	var buf bytes.Buffer
	w := &IVFWriter{w: &buf}

	// 关键帧之前的非关键帧必须被丢弃（否则文件开头无法解码）
	if err := w.WritePacket(rtpPacket(vp8Interframe(20), 100, true)); err != nil {
		t.Fatal(err)
	}
	if w.Frames() != 0 {
		t.Fatalf("关键帧之前不应写出任何帧，实际 %d", w.Frames())
	}

	if err := w.WritePacket(rtpPacket(vp8Keyframe(640, 360, 30), 9000, true)); err != nil {
		t.Fatal(err)
	}
	if err := w.WritePacket(rtpPacket(vp8Interframe(25), 18000, true)); err != nil {
		t.Fatal(err)
	}
	if got := w.Frames(); got != 2 {
		t.Fatalf("应写出 2 帧，实际 %d", got)
	}
	if dw, dh := w.Dimensions(); dw != 640 || dh != 360 {
		t.Fatalf("应从关键帧解析出 640x360，实际 %dx%d", dw, dh)
	}

	raw := buf.Bytes()
	if string(raw[0:4]) != "DKIF" {
		t.Fatalf("IVF 魔数应为 DKIF，实际 %q", raw[0:4])
	}
	if string(raw[8:12]) != "VP80" {
		t.Fatalf("fourcc 应为 VP80，实际 %q", raw[8:12])
	}
	if got := binary.LittleEndian.Uint32(raw[20:24]); got != 90000 {
		t.Fatalf("时间基分母应为 90000（RTP 时钟），实际 %d", got)
	}
	if got := binary.LittleEndian.Uint16(raw[12:14]); got != 640 {
		t.Fatalf("头部宽度应为 640，实际 %d", got)
	}

	// 第一帧记录：长度 + 时间戳（原样写入 RTP 的 90kHz 时间戳）
	if got := binary.LittleEndian.Uint32(raw[32:36]); got != uint32(len(vp8Keyframe(640, 360, 30))) {
		t.Fatalf("第一帧长度记录不正确: %d", got)
	}
	if got := binary.LittleEndian.Uint64(raw[36:44]); got != 9000 {
		t.Fatalf("第一帧时间戳应为 9000，实际 %d", got)
	}
}

// TestFFprobeAcceptsIVF 用真实 ffprobe 验证产物能被识别为 VP8 流。
// 没有 ffprobe 时跳过（CI/服务器上可能没装）——这是**真实**验证，不是"文件非空"。
func TestFFprobeAcceptsIVF(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("未安装 ffprobe，跳过真实容器校验")
	}
	path := filepath.Join(t.TempDir(), "out.ivf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &IVFWriter{w: f}
	ts := uint32(0)
	if err := w.WritePacket(rtpPacket(vp8Keyframe(1280, 720, 200), ts, true)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ts += 3000
		if err := w.WritePacket(rtpPacket(vp8Interframe(120), ts, true)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	out, err := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "stream=codec_name,width,height",
		"-of", "default=noprint_wrappers=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe 无法读取录制文件: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"codec_name=vp8", "width=1280", "height=720"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("ffprobe 输出里应包含 %q，实际:\n%s", want, s)
		}
	}
}

// TestWriterNeverBlocks 钉住"绝不阻塞转发"这条铁律：
// 队列满时丢弃并计数，而不是阻塞调用方。
func TestWriterNeverBlocks(t *testing.T) {
	w := &Writer{frames: make(chan *rtp.Packet, 2)} // 刻意不启动写协程，模拟磁盘卡住
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			w.Write(&rtp.Packet{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter(t):
		t.Fatal("Write 阻塞了——这会拖住媒体转发")
	}
	if w.Dropped() != 8 {
		t.Fatalf("队列容量 2、写入 10 个，应丢弃 8 个，实际 %d", w.Dropped())
	}
}

// TestManagerLifecycle 覆盖文件命名、目录创建与关闭。
func TestManagerLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	m, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Open("room/1", "p:abc", "screen", Codec{Name: "VP8", ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if m.Active() != 1 {
		t.Fatalf("进行中的录制应为 1，实际 %d", m.Active())
	}
	// 文件名必须落在录制目录内，且不含路径分隔符
	if filepath.Dir(w.Path()) != m.Dir() {
		t.Fatalf("录制文件应位于录制目录内，实际 %s", w.Path())
	}
	if !bytes.HasSuffix([]byte(w.Path()), []byte(".ivf")) {
		t.Fatalf("VP8 轨应写 .ivf，实际 %s", w.Path())
	}
	if err := w.ivf.WritePacket(rtpPacket(vp8Keyframe(320, 240, 10), 0, true)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	m.CloseAll()
	if m.Active() != 0 {
		t.Fatalf("关闭后不应有进行中的录制")
	}
	if _, err := os.Stat(w.Path()); err != nil {
		t.Fatalf("录制文件应存在: %v", err)
	}
}

func TestSanitize(t *testing.T) {
	// 断言"属性"而不是具体字符串：真正要保证的是**产物无法逃出录制目录**。
	for _, in := range []string{"room/1", "p:abc", "../../etc", "..", "/", "", "a/../../b"} {
		got := sanitize(in)
		if got == "" {
			t.Fatalf("sanitize(%q) 不应为空", in)
		}
		if strings.ContainsAny(got, "/\\") || strings.Contains(got, "..") {
			t.Fatalf("sanitize(%q) = %q 仍含路径分隔符或 ..，可能写到目录外", in, got)
		}
		// 拼成路径后必须仍在同一目录内
		dir := t.TempDir()
		full := filepath.Join(dir, got+".ivf")
		if filepath.Dir(full) != dir {
			t.Fatalf("sanitize(%q) 拼出的路径逃出了录制目录: %s", in, full)
		}
	}
	if got := sanitize("room/1"); got != "room_1" {
		t.Fatalf("普通房间号应只把 / 换成 _，实际 %q", got)
	}
}

// timeAfter 给"绝不阻塞"的断言一个宽松上限。
func timeAfter(_ *testing.T) <-chan time.Time { return time.After(2 * time.Second) }

// TestIVFDropsCorruptFrames 钉住"丢包时不写残缺帧"：
// 丢一个包（或帧结束包）会让当前帧残缺；若直接写出，播放器会在该帧报错，
// 而且下一帧的数据会被拼进残缺帧里（表现为花屏）。
func TestIVFDropsCorruptFrames(t *testing.T) {
	var buf bytes.Buffer
	w := &IVFWriter{w: &buf}

	// seq=1：关键帧（整帧一个包）
	if err := w.WritePacket(withSeq(rtpPacket(vp8Keyframe(320, 240, 40), 0, true), 1)); err != nil {
		t.Fatal(err)
	}
	if w.Frames() != 1 {
		t.Fatalf("关键帧应写出，实际帧数 %d", w.Frames())
	}

	// seq=2、3：一帧拆两个包（起始 S=1 + 续片 S=0），完整 → 写出
	_ = w.WritePacket(withSeq(rtpPacket(vp8Interframe(30), 3000, false), 2))
	_ = w.WritePacket(withSeq(rtpContinuation(vp8Interframe(30), 3000, true), 3))
	if w.Frames() != 2 {
		t.Fatalf("完整的分片帧应写出，实际帧数 %d", w.Frames())
	}

	// seq=4、6（丢了 5）：残缺 → 必须丢弃
	_ = w.WritePacket(withSeq(rtpPacket(vp8Interframe(30), 6000, false), 4))
	_ = w.WritePacket(withSeq(rtpContinuation(vp8Interframe(30), 6000, true), 6))
	if w.Frames() != 2 {
		t.Fatalf("残缺帧不应写出，实际帧数 %d", w.Frames())
	}
	if w.CorruptFrames() != 1 {
		t.Fatalf("应记录 1 个因丢包丢弃的帧，实际 %d", w.CorruptFrames())
	}

	// seq=7：间隙之后的完整帧仍应正常写出
	_ = w.WritePacket(withSeq(rtpPacket(vp8Interframe(30), 9000, true), 7))
	if w.Frames() != 3 {
		t.Fatalf("间隙之后应恢复录制，实际帧数 %d", w.Frames())
	}
}

// TestOggStructureAndCRC 校验 Ogg 页结构：魔数、页序号递增、**每页 CRC 自校验**。
// CRC 用错（比如用了常见 IEEE CRC32）播放器会整条流拒绝，所以必须验证。
func TestOggStructureAndCRC(t *testing.T) {
	var buf bytes.Buffer
	o := newOggWriter(&buf, 2, 0x1234)
	for i := 0; i < 5; i++ {
		o.WritePacket([]byte{0xfc, byte(i), 0x11, 0x22}, uint32(i*960))
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	raw := buf.Bytes()
	if string(raw[0:4]) != "OggS" {
		t.Fatalf("Ogg 魔数应为 OggS，实际 %q", raw[0:4])
	}
	// 逐页解析：检查页序号递增，并重算 CRC 与页头里存的值比对
	pages, seq := 0, uint32(0)
	for off := 0; off+27 <= len(raw); {
		if string(raw[off:off+4]) != "OggS" {
			t.Fatalf("第 %d 页偏移 %d 处不是 OggS", pages, off)
		}
		stored := binary.LittleEndian.Uint32(raw[off+22 : off+26])
		pageSeq := binary.LittleEndian.Uint32(raw[off+18 : off+22])
		if pageSeq != seq {
			t.Fatalf("页序号应为 %d，实际 %d", seq, pageSeq)
		}
		nsegs := int(raw[off+26])
		size := 27 + nsegs
		for i := 0; i < nsegs; i++ {
			size += int(raw[off+27+i])
		}
		if size <= 0 || off+size > len(raw) {
			t.Fatalf("第 %d 页长度异常: %d", pages, size)
		}
		page := make([]byte, size)
		copy(page, raw[off:off+size])
		binary.LittleEndian.PutUint32(page[22:26], 0) // CRC 字段参与计算时为 0
		if got := oggCRC(page); got != stored {
			t.Fatalf("第 %d 页 CRC 不匹配：页内 %08x，重算 %08x（CRC 算法用错了？）", pages, stored, got)
		}
		off += size
		pages++
		seq++
	}
	if pages < 3 {
		t.Fatalf("应有 OpusHead/OpusTags/数据页 至少 3 页，实际 %d", pages)
	}
}

// TestFFprobeAcceptsOgg 用真实 ffprobe 验证音频容器的头部可被识别为 Opus。
func TestFFprobeAcceptsOgg(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("未安装 ffprobe，跳过真实容器校验")
	}
	path := filepath.Join(t.TempDir(), "out.ogg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	o := newOggWriter(f, 2, 42)
	// 合成的 Opus 包（内容不是真实 Opus，但容器结构必须是合法的）
	for i := 0; i < 60; i++ {
		pkt := make([]byte, 40)
		for j := range pkt {
			pkt[j] = byte(i + j)
		}
		o.WritePacket(pkt, uint32(i*960))
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	out, err := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "stream=codec_name,channels,sample_rate",
		"-of", "default=noprint_wrappers=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe 无法读取 Ogg 录制: %v\n%s", err, out)
	}
	for _, want := range []string{"codec_name=opus", "channels=2", "sample_rate=48000"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("ffprobe 输出应含 %q，实际:\n%s", want, string(out))
		}
	}
}

// withSeq 设置 RTP 序列号（间隙检测依赖它）。
func withSeq(p *rtp.Packet, seq uint16) *rtp.Packet {
	p.SequenceNumber = seq
	return p
}

// TestIVFIgoresOrphanFragments 钉住线上真实成因：
// 大帧被分片后，若**起始分片丢失**，剩下的中间残片仍然带 marker 位。
// 早期实现按 marker 定帧，于是把 0xaa a3 b6... 这种帧中间数据当成整帧写出去，
// 解码器报 "Invalid profile 5"。现在帧起点由 S 位决定，这类残片必须被丢弃。
func TestIVFIgoresOrphanFragments(t *testing.T) {
	var buf bytes.Buffer
	w := &IVFWriter{w: &buf}

	// 先来一个正常关键帧（让文件头/尺寸就位）
	_ = w.WritePacket(withSeq(rtpPacket(vp8Keyframe(640, 360, 50), 0, true), 1))
	if w.Frames() != 1 {
		t.Fatalf("关键帧应写出，实际 %d", w.Frames())
	}

	// 只收到"中间残片"（S=0）并带 marker：绝不能当成一帧写出
	for seq := uint16(2); seq <= 6; seq++ {
		_ = w.WritePacket(withSeq(rtpContinuation(vp8Interframe(40), uint32(seq)*3000, true), seq))
	}
	if w.Frames() != 1 {
		t.Fatalf("孤残片不应写出任何帧，实际帧数 %d（正是线上垃圾帧的成因）", w.Frames())
	}
	if w.OrphanFragments() != 5 {
		t.Fatalf("应记录 5 个孤残片，实际 %d", w.OrphanFragments())
	}

	// 之后正常的完整帧仍应写出（录制能自愈）
	_ = w.WritePacket(withSeq(rtpPacket(vp8Interframe(40), 21000, true), 7))
	if w.Frames() != 2 {
		t.Fatalf("恢复后应继续写出，实际帧数 %d", w.Frames())
	}
}

// TestRealCaptureRegression 用**真实抓包的夹具**做回归。
//
// 为什么不用手造数据：手造的关键帧能过单元测试，却过不了真解码——
// 这次的两个真实缺陷（帧中间残片被当成整帧、Ogg 收尾写出空包页）
// 都只有真实数据才暴露得出来。夹具取自一次真实浏览器会话（testdata/）。
//
// 断言三件事：装配出的每一帧都是合法 VP8、没有残缺/孤残片造成的坏帧、
// 且 ffmpeg 能**完整解码**重放结果（有解码错误就失败）。
func TestRealCaptureRegression(t *testing.T) {
	const dumpPath = "testdata/real-vp8-camera.rtp"
	h, packets, _, err := ReadRawDump(dumpPath)
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	if h.Codec != "VP8" || len(packets) == 0 {
		t.Fatalf("夹具元信息异常: %+v", h)
	}

	out := filepath.Join(t.TempDir(), "replay.ivf")
	stats, err := AnalyzeDump(dumpPath, out)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if stats.Written == 0 {
		t.Fatalf("重放没有写出任何帧: %+v", stats)
	}
	// 装配后不应再有"关键帧却缺起始码"的坏帧——这正是修好的缺陷
	if stats.KeyHeaderBad != 0 {
		t.Fatalf("装配出的关键帧仍有 %d 个缺起始码（帧起点判断错了）", stats.KeyHeaderBad)
	}
	if stats.InterHeaderBad != 0 {
		t.Fatalf("有 %d 个非关键帧首字节非法", stats.InterHeaderBad)
	}

	// 真实数据里必然存在"起始分片丢失、只剩中间残片"的包；它们必须被丢弃而不是写出
	if stats.OrphanFragments == 0 {
		t.Log("本次夹具里没有出现孤残片（不影响结论）")
	}

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("未安装 ffmpeg，跳过完整解码校验")
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", out, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg 解码失败: %v\n%s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("ffmpeg 报告了解码错误（说明帧装配仍有问题）:\n%s", stderr.String())
	}
}
