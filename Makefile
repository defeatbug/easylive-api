# 开发环境便捷命令。
#   make up / down / logs / psql  → 在【宿主机】执行（devcontainer 内没有 docker CLI）
#   make run / gen / test / build → 在【devcontainer 内】执行

.DEFAULT_GOAL := help
SHELL := /bin/sh

POSTGRES_USER ?= postgres
POSTGRES_DB_NAME ?= sfu_dev

.PHONY: help up down logs psql run gen test build lint

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

up: ## [宿主机] 启动 postgres 等基础服务
	docker compose up -d

down: ## [宿主机] 停止基础服务
	docker compose down

logs: ## [宿主机] 跟踪基础服务日志
	docker compose logs -f

psql: ## [宿主机] 进入数据库
	docker compose exec postgresql psql -U $(POSTGRES_USER) -d $(POSTGRES_DB_NAME)

run: ## [容器内] 运行服务
	go run .

gen: ## [容器内] 生成 ent / gqlgen 代码
	go generate ./...

test: ## [容器内] 单元测试
	go test ./...

build: ## [容器内] 构建二进制
	go build -o /tmp/easylive-api .

lint: ## [容器内] go vet
	go vet ./...
