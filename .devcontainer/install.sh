#!/bin/sh
# 容器创建后执行一次（幂等）。基础工具大多已在 Dockerfile 里装好，这里只做补充与国内镜像源切换。

# 如果不需要修改 apt 的源，注释下面 2 行
# cp /etc/apt/sources.list /etc/apt/sources.list.bak
# sed -i 's/deb.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list

apt-get update -y
apt-get install -y iputils-ping unzip tig

# 国内网络下加速 Go module 下载（不需要就删掉）
go env -w GOPROXY=https://goproxy.cn,direct

echo "✅ devcontainer init done"
