# easylive 生产部署（宿主机二进制）

说明
1. 在构建机上执行 `bash build.sh`（产出 `build/bin/easylive-api`）和 `bash fetch-ai-assets.sh`（Piper + 模型到 `build/ai/`）
2. 把 `deployment-example/` 复制到服务器 `/opt/easylive/`，再把二进制、AI 产物、迁移文件传过去
3. 在服务器上复制 `./api/.env.example` 到 `./api/.env`，修改相关配置
4. 查看 `./api/README.md`（后端 + systemd + postgres + 验收）
5. 查看 `./web/README.md`（前端 + nginx）
