#!/bin/bash
# easylive-api 部署构建：交叉编译 linux/amd64 静态二进制。
#
# 用法（构建机上执行，不需要在服务器上装 Go）：
#   ./build.sh
#
# 产物：build/bin/easylive-api（CGO_ENABLED=0，纯静态，可直接 scp 到服务器）。
# 目标架构固定为 linux/amd64（生产服务器是 x86_64；换架构改下面的 GOARCH）。
set -e
cd "$(dirname "$0")"

echo "1. 环境变量 ------------------------------"
export GOPROXY="https://goproxy.cn,https://goproxy.io,direct"
export GO111MODULE=on
export CGO_ENABLED=0
export GOOS=linux
export GOARCH=amd64

echo "2. 清理构建 ------------------------------"
rm -rf build/bin/
mkdir -p build/bin/

echo "3. 构建开始 ------------------------------"
go build -trimpath -ldflags '-s -w' -o build/bin/easylive-api . && echo -e "\t构建完成 build/bin/easylive-api"

echo "4. 构建完成 ------------------------------"
ls -l build/bin/
