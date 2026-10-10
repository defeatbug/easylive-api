#!/usr/bin/env bash
# 实验矩阵：按路数 × 码率 依次运行压测，每个组合一个结果目录。
#
# 用法：
#   LANES="2 4 8" VIDEO_KBPS="800 1500" ./scripts/matrix.sh
#
# 依赖：先启动被测服务端；压测端与服务端**应分机器**（同机会让 CPU 数据互相污染）。
set -euo pipefail

WS="${WS:-ws://127.0.0.1:8080/ws}"
ROOM="${ROOM:-matrix}"
DURATION="${DURATION:-30s}"
LANES="${LANES:-2 4 8}"
VIDEO_KBPS="${VIDEO_KBPS:-800 1500}"
AUDIO_KBPS="${AUDIO_KBPS:-64}"
FPS="${FPS:-30}"
OUT="${OUT:-results}"
REPEAT="${REPEAT:-1}"   # 正式实验建议 >= 10

for lanes in $LANES; do
  for kbps in $VIDEO_KBPS; do
    for rep in $(seq 1 "$REPEAT"); do
      tag="${lanes}lanes-${kbps}kbps-r${rep}"
      echo "=== 运行 $tag ==="
      go run ./cmd/loadtest \
        -ws "$WS" -room "$ROOM" \
        -lanes "$lanes" -duration "$DURATION" \
        -video-kbps "$kbps" -audio-kbps "$AUDIO_KBPS" -fps "$FPS" \
        -out "$OUT" -tag "$tag"
    done
  done
done

echo
echo "全部完成。结果目录：$OUT/"
echo "下一步：把各 summary.json 汇总成图表（论文用）。"
