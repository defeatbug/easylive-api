package mediarec

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// rawDump 把一个轨道的**原始 RTP 包**原样落盘，用于离线分析。
//
// 为什么需要它：录制管道的缺陷（帧拼装、时间戳、容器合规）只能靠真实数据定位。
// 有了转储，就可以在本地用 cmd/rtpfix 反复重放 + ffmpeg 校验，几秒一次，
// 不必每次都重启服务、开浏览器、等宽限期。
//
// 文件格式（自描述，逐包定长头）：
//
//	ELRTP1\n
//	{...codec 元信息...}\n
//	重复： [8B 接收时刻 UnixMicro][4B 长度][原始 RTP 包]
type rawDump struct {
	f  *os.File
	bw *bufio.Writer
	mu sync.Mutex
}

// rawDumpHeader 是转储文件里的元信息（分析时用来重建写入器）。
type rawDumpHeader struct {
	Codec       string `json:"codec"`
	ClockRate   uint32 `json:"clockRate"`
	PayloadType uint8  `json:"payloadType"`
	SSRC        uint32 `json:"ssrc"`
	Room        string `json:"room"`
	Participant string `json:"participant"`
	Source      string `json:"source"`
	Kind        string `json:"kind"`
}

const rawDumpMagic = "ELRTP1"

func newRawDump(path string, h rawDumpHeader) (*rawDump, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	d := &rawDump{f: f, bw: bufio.NewWriterSize(f, 1<<16)}
	if _, err := d.bw.WriteString(rawDumpMagic + "\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	meta, err := json.Marshal(h)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := d.bw.Write(append(meta, '\n')); err != nil {
		_ = f.Close()
		return nil, err
	}
	return d, nil
}

// write 追加一个包（含接收时刻，便于分析抖动与丢包时序）。
func (d *rawDump) write(pkt *rtp.Packet) {
	if d == nil {
		return
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return
	}
	var hdr [12]byte
	binary.BigEndian.PutUint64(hdr[0:8], uint64(time.Now().UnixMicro()))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(raw)))
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.bw.Write(hdr[:]); err != nil {
		return
	}
	_, _ = d.bw.Write(raw)
}

func (d *rawDump) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.bw.Flush()
	return d.f.Close()
}

// ReadRawDump 读取转储文件，返回元信息与按顺序排列的包。
// 供离线分析使用（cmd/rtpfix 与测试夹具）。
func ReadRawDump(path string) (rawDumpHeader, []*rtp.Packet, []time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return rawDumpHeader{}, nil, nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	magic, err := br.ReadString('\n')
	if err != nil {
		return rawDumpHeader{}, nil, nil, fmt.Errorf("读取转储头失败: %w", err)
	}
	if len(magic) < len(rawDumpMagic) || magic[:len(rawDumpMagic)] != rawDumpMagic {
		return rawDumpHeader{}, nil, nil, fmt.Errorf("不是 easylive RTP 转储文件（魔数不符）")
	}
	metaLine, err := br.ReadString('\n')
	if err != nil {
		return rawDumpHeader{}, nil, nil, fmt.Errorf("读取转储元信息失败: %w", err)
	}
	var h rawDumpHeader
	if err := json.Unmarshal([]byte(metaLine), &h); err != nil {
		return rawDumpHeader{}, nil, nil, fmt.Errorf("解析转储元信息失败: %w", err)
	}

	var (
		packets []*rtp.Packet
		times   []time.Duration
		first   time.Time
	)
	var hdr [12]byte
	for {
		if _, err := readFull(br, hdr[:]); err != nil {
			break // 文件结束
		}
		us := binary.BigEndian.Uint64(hdr[0:8])
		n := binary.BigEndian.Uint32(hdr[8:12])
		if n == 0 || n > 1<<16 {
			return h, packets, times, fmt.Errorf("转储损坏：包长度 %d", n)
		}
		buf := make([]byte, n)
		if _, err := readFull(br, buf); err != nil {
			return h, packets, times, fmt.Errorf("转储截断于第 %d 个包", len(packets)+1)
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf); err != nil {
			return h, packets, times, fmt.Errorf("第 %d 个包无法解析: %w", len(packets)+1, err)
		}
		t := time.UnixMicro(int64(us))
		if first.IsZero() {
			first = t
		}
		packets = append(packets, pkt)
		times = append(times, t.Sub(first))
	}
	return h, packets, times, nil
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
