#!/bin/bash
# 在服务器上编译 whisper-cli（服务器上原生编译；约几分钟）。
#
# 用法（服务器上执行）：
#   sudo ./build-whisper.sh [whisper.cpp 源码目录，默认 /tmp/whisper.cpp] [安装目录，默认 /opt/easylive/api/bin]
#
# whisper 只在本地转写时用（录音转写、后续的纪要功能）；v1 问答助手不依赖它，
# 可以先不装，需要时再跑这个脚本。AI 资产缺失不影响开会。
set -e

WORK="${1:-/tmp/whisper.cpp}"
DEST="${2:-/opt/easylive/api/bin}"

echo "1. 安装构建依赖 ------------------------------"
sudo apt update && sudo apt install -y git cmake g++ make

echo "2. 获取 whisper.cpp 源码 ------------------------------"
if [ ! -d "$WORK" ]; then
  git clone --depth 1 https://github.com/ggml-org/whisper.cpp.git "$WORK"
else
  echo "源码目录已存在，跳过 clone：$WORK"
fi

echo "3. 编译 whisper-cli ------------------------------"
cmake -S "$WORK" -B "$WORK/build" -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON
cmake --build "$WORK/build" -j"$(nproc)" --target whisper-cli

echo "4. 安装 ------------------------------"
mkdir -p "$DEST"
cp "$WORK/build/bin/whisper-cli" "$DEST/whisper-cli"
ls -l "$DEST/whisper-cli"
echo "whisper-cli 已安装到 $DEST/whisper-cli"
