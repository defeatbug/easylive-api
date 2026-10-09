# 仅服务开发环境（沿用 egg-api 的 devcontainer 约定）。生产部署请用仓库根目录下的多阶段 Dockerfile，不要复用本文件。
# ⚠️ GOLANG_VERSION 必须与 api/go.mod 的 go 指令兼容（本仓库 go.mod 写的是 go 1.25）。
ARG GOLANG_VERSION=1.25
ARG IMAGE_BASE="golang:${GOLANG_VERSION}-bookworm"

FROM ${IMAGE_BASE} AS dev

# 基础工具：git/curl 拉依赖，iproute2 提供 tc（弱网注入 netem），tcpdump 抓包分析 RTP
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git curl ca-certificates \
        iputils-ping unzip tig less \
        netcat-openbsd tcpdump iproute2 \
    && rm -rf /var/lib/apt/lists/*

# Go 工具链：dlv 断点调试、air 热重载、gqlgen/ent 代码生成
RUN go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install github.com/air-verse/air@latest \
    && go install github.com/99designs/gqlgen@latest \
    && go install entgo.io/ent/cmd/ent@latest

ENV CGO_ENABLED=0 \
    GOPATH=/go \
    PATH=/go/bin:/usr/local/go/bin:$PATH
