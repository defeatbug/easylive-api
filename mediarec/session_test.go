package mediarec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
)

// TestSessionManifestAndList 覆盖一个完整录制会话的生命周期：
// 开始 → 各轨道落盘 → 结束 → 清单写到磁盘 → ListSessions 能读回来。
//
// 为什么值得测：清单是"录过什么"的唯一真相来源，服务重启后列表就靠它。
// 一旦清单写入/统计回填出错，用户会看到"录了 0 帧/0 秒"这种不可信的数据。
func TestSessionManifestAndList(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	sess, err := m.StartSession("room-1", "p-admin")
	if err != nil {
		t.Fatal(err)
	}
	// 重复开始应当幂等（主持人在前端连点两下不该开出两个会话）
	if again, _ := m.StartSession("room-1", "p-admin"); again.ID != sess.ID {
		t.Fatalf("重复开始应为同一会话，实际 %s vs %s", again.ID, sess.ID)
	}
	if !m.Recording() {
		t.Fatal("会话开始后 Recording() 应为 true")
	}

	// 视频轨：5 帧，时间戳跨 36000（90kHz → 0.4s）
	vw, err := m.Open("room-1", "p-1", "camera", Codec{Name: "VP8", ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		vw.Write(rtpPacket(vp8Keyframe(640, 360, 40), uint32(i*9000), true))
	}
	if err := vw.Close(); err != nil {
		t.Fatal(err)
	}

	// 音频轨：两个包（20ms 间隔 → 约 20ms 时长）
	aw, err := m.Open("room-1", "p-1", "microphone", Codec{Name: "opus", ClockRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	aw.Write(&rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0xfc, 0x01, 0x02}})
	aw.Write(&rtp.Packet{Header: rtp.Header{Timestamp: 960}, Payload: []byte{0xfc, 0x03, 0x04}})
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}

	stopped := m.StopSession()
	if stopped == nil || stopped.EndedAt == nil {
		t.Fatal("停止后应带回 EndedAt")
	}
	if m.Recording() {
		t.Fatal("停止后 Recording() 应为 false")
	}

	// 磁盘上的清单
	raw, err := os.ReadFile(filepath.Join(dir, sess.ID, "manifest.json"))
	if err != nil {
		t.Fatalf("清单未落盘: %v", err)
	}
	var got Session
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("清单不可解析: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("应登记 2 条轨道文件，实际 %d：%+v", len(got.Files), got.Files)
	}
	var video, audio *FileInfo
	for i := range got.Files {
		switch got.Files[i].Kind {
		case "video":
			video = &got.Files[i]
		case "audio":
			audio = &got.Files[i]
		}
	}
	if video == nil || audio == nil {
		t.Fatalf("应同时有 video/audio 两种文件，实际 %+v", got.Files)
	}
	if video.Frames != 5 {
		t.Fatalf("视频应记录 5 帧，实际 %d", video.Frames)
	}
	if video.SizeBytes <= 0 {
		t.Fatalf("视频文件大小应大于 0，实际 %d", video.SizeBytes)
	}
	if video.DurationMs < 350 || video.DurationMs > 450 {
		t.Fatalf("视频时长应约 400ms（由 RTP 时间戳跨度算出），实际 %dms", video.DurationMs)
	}
	if audio.Frames != 2 {
		t.Fatalf("音频应记录 2 个包，实际 %d", audio.Frames)
	}
	if video.FfmpegHint == "" || audio.FfmpegHint == "" {
		t.Fatal("应给出零转码的 ffmpeg 提示命令")
	}

	// ListSessions 从磁盘读回，并按磁盘真实大小校正
	list, err := m.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应列出 1 个会话，实际 %d", len(list))
	}
	if list[0].ID != sess.ID || list[0].RoomID != "room-1" || list[0].StartedBy != "p-admin" {
		t.Fatalf("会话元信息不正确: %+v", list[0])
	}
	for _, f := range list[0].Files {
		if info, err := os.Stat(filepath.Join(dir, sess.ID, f.Name)); err == nil && f.SizeBytes != info.Size() {
			t.Fatalf("磁盘上 %s 大小应为 %d，清单里是 %d", f.Name, info.Size(), f.SizeBytes)
		}
	}
}

// TestSessionPathSafety 钉住路径穿越防护：API 会用这些名字拼路径。
func TestSessionPathSafety(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../etc/passwd", "..", "a/b", "", "a\\b", "x..y"} {
		if _, ok := m.SessionPath(bad, "manifest.json"); ok {
			t.Fatalf("会话名 %q 不应被接受", bad)
		}
		if _, ok := m.SessionPath("20261013-101500-room", bad); ok {
			t.Fatalf("文件名 %q 不应被接受", bad)
		}
	}
	if p, ok := m.SessionPath("20261013-101500-room", "150405.000-room-p-camera.ivf"); !ok {
		t.Fatalf("正常文件名应被接受，实际被拒绝")
	} else if filepath.Dir(filepath.Dir(p)) != m.Dir() {
		t.Fatalf("解析出的路径应位于录制根目录下: %s", p)
	}
}

// TestStopWithoutStart 幂等性：没在录时停止应当安全返回 nil。
func TestStopWithoutStart(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s := m.StopSession(); s != nil {
		t.Fatalf("未开始录制时停止应返回 nil，实际 %+v", s)
	}
	if m.Recording() {
		t.Fatal("未开始录制时 Recording() 应为 false")
	}
	if list, err := m.ListSessions(); err != nil || len(list) != 0 {
		t.Fatalf("空目录应列出 0 个会话，实际 %d err=%v", len(list), err)
	}
}
