# 开发环境便捷命令。
#   make up / down / logs / psql  → 在【宿主机】执行（devcontainer 内没有 docker CLI）
#   make run / gen / test / build → 在【devcontainer 内】执行

.DEFAULT_GOAL := help
SHELL := /bin/sh

POSTGRES_USER ?= postgres
POSTGRES_DB_NAME ?= sfu_dev

.PHONY: help up down logs psql run gen test build lint prod-up prod-down prod-logs prod-ps prod-check turn-check

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

# ---- 生产（在服务器上执行；compose 文件在 deploy/）----
COMPOSE_PROD := docker compose -f deploy/compose.prod.yml --env-file deploy/.env.prod

prod-up: ## [服务器] 构建并启动 api + web + postgres + coturn
	$(COMPOSE_PROD) up -d --build

prod-down: ## [服务器] 停止生产编排（保留数据卷）
	$(COMPOSE_PROD) down

prod-logs: ## [服务器] 跟踪生产日志
	$(COMPOSE_PROD) logs -f --tail=100

prod-ps: ## [服务器] 查看生产容器状态
	$(COMPOSE_PROD) ps

prod-check: ## [服务器] 验收：健康检查 + ICE 配置是否下发 TURN 凭证
	@curl -fsS "https://$(DOMAIN)/healthz" && echo
	@curl -fsS "https://$(DOMAIN)/ice?identity=smoke" && echo

turn-check: ## [服务器] 用 coturn 自带客户端验证中继（需要 SECRET/IP 变量）
	@test -n "$(SECRET)" && test -n "$(IP)" || (echo "用法：make turn-check SECRET=<TURN_SECRET> IP=<公网IP>"; exit 1)
	@U="$$(($$(date +%s)+3600)):smoke"; \
	 C="$$(printf '%s' "$$U" | openssl dgst -sha1 -hmac "$(SECRET)" -binary | base64)"; \
	 docker run --rm coturn/coturn:4.6 turnutils_uclient -T -u "$$U" -w "$$C" -p 3478 "$(IP)"

