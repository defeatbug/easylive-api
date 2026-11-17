#!/bin/bash
# 下载 AI 语音所需的预编译产物（arm64）：Piper 二进制 + 语音模型 + Whisper 模型。
#
# 用法（构建机或服务器上执行均可）：
#   ./fetch-ai-assets.sh [输出目录，默认 build/ai]
#
# 产物：
#   <输出目录>/piper/   Piper 整个目录（piper 二进制 + espeak-ng-data，须原样保留，
#                       espeak-ng-data 靠相对路径定位，不能只拷二进制）
#   <输出目录>/models/  ggml-small.bin、zh_CN-huayan-medium.onnx、zh_CN-huayan-medium.onnx.json
#
# 注意：whisper-cli 不在这里下载——它没有官方 arm64 预编译包，
# 须在服务器上编一次，见 deployment-example/api/build-whisper.sh。
# AI 资产缺失不影响开会（只影响 AI 转写/语音功能，缺失时 AI 会话明确拒绝并记日志）。
set -e
cd "$(dirname "$0")"

OUT="${1:-build/ai}"
mkdir -p "$OUT/piper" "$OUT/models"

PIPER_VER="2023.11.14-2"

echo "1. Piper (linux_aarch64) ------------------------------"
curl -fL -o /tmp/piper.tar.gz \
  "https://github.com/rhasspy/piper/releases/download/${PIPER_VER}/piper_linux_aarch64.tar.gz"
# 压缩包顶层是个 piper/ 目录；--strip-components=1 去掉它，让二进制与
# espeak-ng-data 直接落在 $OUT/piper/ 下（与 .env 里的 PIPER_BIN 路径一致）
tar xzf /tmp/piper.tar.gz -C "$OUT/piper" --strip-components=1
rm -f /tmp/piper.tar.gz
echo -e "\tPiper 完成 $OUT/piper"

echo "2. Piper 中文语音模型（约 63MB） ------------------------------"
curl -fL -o "$OUT/models/zh_CN-huayan-medium.onnx" \
  "https://huggingface.co/rhasspy/piper-voices/resolve/main/zh/zh_CN/huayan/medium/zh_CN-huayan-medium.onnx"
curl -fL -o "$OUT/models/zh_CN-huayan-medium.onnx.json" \
  "https://huggingface.co/rhasspy/piper-voices/resolve/main/zh/zh_CN/huayan/medium/zh_CN-huayan-medium.onnx.json"
echo -e "\t语音模型完成 $OUT/models"

echo "3. Whisper 模型 ggml-small.bin（约 500MB） ------------------------------"
curl -fL -o "$OUT/models/ggml-small.bin" \
  "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin"
echo -e "\tWhisper 模型完成 $OUT/models"

echo "4. 全部完成 ------------------------------"
ls -lR "$OUT"
