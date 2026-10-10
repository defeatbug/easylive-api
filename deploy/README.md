# 生产部署（单节点）

一条命令起 `api + web + postgres + coturn`，全站 HTTPS/WSS。目标机器：**4 vCPU / 8 GB** 云服务器（Ubuntu 22.04+ 或同级别）。

> 本文档里的命令都在**服务器**上执行。本地开发不需要看这份文档。

## 0. 目录约定

两个仓库克隆在**同级目录**下（compose 里用相对路径引用前端源码）：

```bash
git clone https://github.com/defeatbug/easylive-api.git ~/easylive-api
git clone https://github.com/defeatbug/easylive-web.git ~/easylive-web
```

## 1. 安全组 / 防火墙端口

| 端口 | 协议 | 用途 |
|---|---|---|
| 22 | TCP | SSH |
| 80 | TCP | HTTP → HTTPS 跳转、certbot 签发 |
| 443 | TCP | HTTPS + WSS 信令 |
| **7881** | **UDP** | **单端口媒体 mux（音视频 RTP 全走这一个口）** |
| 7882 | TCP | ICE-TCP 兜底（UDP 被封的网络） |
| 3478 | UDP+TCP | coturn 中继 |
| 49160–49200 | UDP | coturn 中继端口段 |

漏开 **7881/udp** 是最常见的故障：信令连得上、画面永远黑。

## 2. 签发证书

```bash
sudo apt update && sudo apt install -y certbot
sudo certbot certonly --standalone -d sfu.example.com   # 域名先解析到本机公网 IP
# 拷贝到 compose 挂载的目录（续期后重复这一步，或做软链）
sudo mkdir -p ~/easylive-api/deploy/certs
sudo cp /etc/letsencrypt/live/sfu.example.com/fullchain.pem ~/easylive-api/deploy/certs/
sudo cp /etc/letsencrypt/live/sfu.example.com/privkey.pem  ~/easylive-api/deploy/certs/
```

## 3. 配置环境变量

```bash
cd ~/easylive-api
cp deploy/.env.prod.example deploy/.env.prod
# 生成密钥（三个都要换）
openssl rand -base64 48   # → JWT_SECRET
openssl rand -base64 48   # → TURN_SECRET（coturn 用同一个值）
openssl rand -hex 24      # → TOKEN_API_KEY
# 必填：SFU_ADVERTISE_IP、TURN_EXTERNAL_IP、TURN_URLS、WEB_ORIGIN、POSTGRES_PASSWORD
```

**`SFU_ADVERTISE_IP` 必须填公网 IP**：云服务器自动枚举到的是内网地址，浏览器拿到 `172.x` 候选必然连不通。

## 4. 启动

```bash
cd ~/easylive-api
docker compose -f deploy/compose.prod.yml --env-file deploy/.env.prod up -d --build
docker compose -f deploy/compose.prod.yml ps
```

## 5. 验收清单（逐条都要过）

```bash
# ① 服务健康
curl -fsS https://sfu.example.com/healthz

# ② ICE 配置里必须带上 TURN 临时凭证（turnEnabled=true 且 credential 非空）
curl -fsS "https://sfu.example.com/ice?identity=smoke"

# ③ 中继真的能用：用 openssl 独立算一份凭证，喂给 coturn 自带客户端
SECRET='你的 TURN_SECRET'; IP='你的公网IP'
USER="$(($(date +%s)+3600)):smoke"
CRED=$(printf '%s' "$USER" | openssl dgst -sha1 -hmac "$SECRET" -binary | base64)
docker run --rm coturn/coturn:4.6 turnutils_uclient -T -u "$USER" -w "$CRED" -p 3478 "$IP"
# 期望：输出 relay 分配成功、收发若干包。若报 401，说明静态密钥与 /ice 用的不是同一个
```

④ 浏览器打开 `https://sfu.example.com` → 会前页能看到自己（说明 HTTPS 与设备权限没问题）
⑤ 两个浏览器（或手机）入会 → 互相看得到、听得到
⑥ 打开界面右侧**指标**面板 → 看「候选类型」：直连显示 `host`/`srflx`，走中继显示 **`relay`**
⑦ 屏幕共享：主画面 + 右下角可拖动小窗
⑧ 断线重连：关掉 Wi-Fi 再打开，应自动恢复（服务端宽限期 30s）

## 6. 故障对照表

| 现象 | 最可能的原因 |
|---|---|
| 页面打不开 / 证书错误 | 证书路径不对；`deploy/certs` 没挂上；域名没解析 |
| 信令连不上（WSS 400） | Nginx 少了 `Upgrade`/`Connection` 头 |
| **信令正常但永远黑屏** | **7881/udp 没开**，或 `SFU_ADVERTISE_IP` 填了内网地址 |
| 局域网能通、跨网必失败 | TURN 没配好：`/ice` 里 `turnEnabled=false`，或 coturn 的 `static-auth-secret` 与 `TURN_SECRET` 不一致 |
| 60 秒左右必掉线 | Nginx 的 `proxy_read_timeout` 用了默认值（本配置已设为 3600s） |
| 入会被拒且提示缺少令牌 | `AUTH_MODE=required` 下前端没取到令牌（检查 `/token` 是否被反代） |
| 摄像头打不开 | 不是 HTTPS（安全上下文要求），或 `Permissions-Policy` 写错了 |

## 7. 和评测章节的衔接

- 平时 `ENABLE_DEBUG_ENDPOINT=false`；**跑实验时临时改成 `true`** 并重启 api，
  即可用 `/debug/stats` 拿到每个参与者的 `receivedFromPublisher` / `published.packets` /
  `sentToSubscriber` 三组计数，用来区分"发布端没出帧 / 服务端没转发 / 转发到订阅端失败"。
- 带宽对比实验：静音后服务端**停止转发该音频轨**（`dropped` 计数会增长），
  所以"静音省多少带宽"可以直接量出来，不需要估算。
- 中继 vs 直连对比：把 `TURN_URLS` 临时改成只给 `?transport=udp` 并用防火墙丢掉 7881/udp，
  即可强制走中继，用指标面板的 `candidateType` 确认，再对比 RTT / 码率。

## 8. 安全提醒

- `deploy/.env.prod`、`deploy/certs/` 都已在 `.gitignore` 里，**不要提交**。
- `/token` 在 `APP_ENV=production` 下必须带 `X-API-Key`，但它仍然是"能拿到 Key 就能自签令牌"。
  正式上线应当把它替换成你自己的登录鉴权（校验账号密码 / SSO 后再签发）。
- 定期轮换 `JWT_SECRET`（旧令牌会立即失效）与 `TURN_SECRET`（需与 coturn 同步改）。
