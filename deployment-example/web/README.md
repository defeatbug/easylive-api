# 前端部署

1. 在 easylive-web 仓库构建：
   ```bash
   pnpm install
   pnpm build        # 产物在 dist/
   ```
2. 把 dist/ 的内容复制到服务器 `/opt/easylive/web/`：
   ```bash
   scp -r dist/* user@10.1.0.202:/opt/easylive/web/
   ```
3. 宿主机 nginx（见 `../nginx/easylive.conf`）伺服该目录；前端与 API 同源（页面里的
   `/ws`、`/token`、`/ice` 都是相对地址），无需额外配置。
4. NPM 把 `hjy.dmmcloud.com` 指到宿主机 nginx 的 80 端口（TLS 由 NPM 终止）。

注意：前后端一起更新（client_metrics 是配对的协议改动，只更一端会不兼容）。
