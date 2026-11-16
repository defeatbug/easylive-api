# easylive 生产部署（宿主机二进制 + systemd + nginx）

服务器：Linux arm64，systemd。应用以二进制直接跑在宿主机上，不走 docker；
只有 postgres 还在 docker 里（一个容器）。

## 服务器目录结构

```
/opt/easylive/
├── api/
│   ├── bin/
│   │   ├── easylive-api      # build.sh 产物
│   │   └── whisper-cli       # build-whisper.sh 产物（可选，见第 5 节）
│   ├── piper/
│   │   ├── piper             # fetch-ai-assets.sh 产物（整个目录原样保留，
│   │   └── espeak-ng-data/   # espeak-ng-data 靠相对路径定位，不能只拷二进制）
│   ├── migrations/           # 从仓库 migrations/ 复制
│   ├── models/
│   │   ├── ggml-small.bin
│   │   ├── zh_CN-huayan-medium.onnx
│   │   └── zh_CN-huayan-medium.onnx.json
│   ├── recordings/           # 运行时自动生成
│   └── .env                  # 从 .env.example 复制并填写
├── web/                      # 前端 dist 内容（见 ../web/README.md）
└── nginx/
    └── easylive.conf
```

## 1. 构建（构建机上执行，不需要在服务器上装 Go）

```bash
./build.sh             # 产出 build/bin/easylive-api（linux/arm64 静态二进制）
./fetch-ai-assets.sh   # 下载 Piper arm64 + 语音模型 + Whisper 模型到 build/ai/
```

## 2. 传到服务器（scp）

```bash
# 部署文件（第一次；之后只在改了部署文件时重传）
scp -r deployment-example/* user@10.1.0.202:/opt/easylive/

# 二进制 + AI 产物 + 迁移文件
scp build/bin/easylive-api user@10.1.0.202:/opt/easylive/api/bin/
scp -r build/ai/piper user@10.1.0.202:/opt/easylive/api/
scp -r build/ai/models user@10.1.0.202:/opt/easylive/api/
scp -r migrations user@10.1.0.202:/opt/easylive/api/
```

日常更新只用传新二进制并重启服务（见第 7 节），不用重传全部。

## 3. 配置 .env（服务器上）

```bash
cp /opt/easylive/api/.env.example /opt/easylive/api/.env
# 编辑 /opt/easylive/api/.env，必填：
#   JWT_SECRET、TOKEN_API_KEY（openssl rand 生成，见 .env.example 开头）
#   SFU_ADVERTISE_IP=10.1.0.202,2607:5501:3000:14ad::2
#   POSTGRES_PASSWORD（必须与已有数据卷一致，见第 4 节）
#   DEEPSEEK_API_KEY（要用 AI 问答/纪要时填）
```

## 4. postgres（docker 里只剩它一个容器）

```bash
# 数据目录：旧数据还在就沿用它（密码必须用旧的）：
#   ~/easylive-api_volumes/postgresql_volume/data
# 全新部署用 /opt/easylive/postgresql/data。
docker run -d --name easylive-postgresql --restart unless-stopped \
  -p 127.0.0.1:5432:5432 \
  -v /opt/easylive/postgresql/data:/var/lib/postgresql/data \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=<与 .env 里一致> \
  -e POSTGRES_DB=sfu_prod \
  postgres:17.2-alpine3.20
```

数据库是可选依赖：连不上时会议照常进行，只是 /graphql 与指标落库不可用。

## 5. whisper-cli（可选，服务器上编一次）

whisper 只在本地转写时用（录音转写、后续的纪要功能）；v1 问答助手不依赖它，
可以先不装。需要时在服务器上跑：

```bash
cd /opt/easylive/api && sudo ./build-whisper.sh
```

## 6. systemd 服务 + nginx

```bash
# 服务
sudo cp /opt/easylive/api/easylive-api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now easylive-api
journalctl -u easylive-api -f        # 看日志

# nginx（先装好 nginx：sudo apt install -y nginx）
sudo ln -s /opt/easylive/nginx/easylive.conf /etc/nginx/sites-enabled/easylive.conf
sudo nginx -t && sudo systemctl reload nginx

# NPM：把 hjy.dmmcloud.com 的上游从 http://10.1.0.202:3000 改成 http://10.1.0.202:80
# 防火墙：sudo ufw allow 7881/udp；sudo ufw allow 7882/tcp
```

## 7. 日常更新

```bash
# 构建机：./build.sh
# 传新二进制并重启：
scp build/bin/easylive-api user@10.1.0.202:/opt/easylive/api/bin/
ssh user@10.1.0.202 'sudo systemctl restart easylive-api'
# 前后端一起更新（client_metrics 是配对的协议改动）：前端见 ../web/README.md
```

## 8. 验收清单（逐条都要过）

```bash
# ① 服务健康
curl -fsS http://127.0.0.1:3000/healthz && echo
# ② ICE 配置下发（含 STUN；TURN 留空时不含 turn）
curl -fsS "http://127.0.0.1:3000/ice?identity=smoke" && echo
# ③ 启动日志里有广播地址
journalctl -u easylive-api --since "5 min ago" | grep "广播 ICE 候选地址"
# 应看到：sfu: 广播 ICE 候选地址 [10.1.0.202 2607:5501:3000:14ad::2]（udp/7881 tcp/7882）
# ④ 浏览器打开 https://hjy.dmmcloud.com 进房间，两边能看到对方视频
```

## 9. 常见问题

| 现象 | 原因 / 处理 |
|---|---|
| 服务起不来，`SESSION_SECRET 显式配置时至少 16 字符` | .env 里 SESSION_SECRET 填短了；删掉它（留空回落到 JWT_SECRET）或改成 16 位以上 |
| 日志 `数据层未启用` | postgres 没起来或密码不对；先 `docker ps` 看容器，再核对 .env 里的 POSTGRES_PASSWORD 与数据卷是否一致 |
| 信令连得上、画面永远黑 | 7881/udp 没放行，或 SFU_ADVERTISE_IP 没填公网 IPv6；看第 8 节第 ③ 条日志确认广播地址 |
| AI 问答报未就绪 | DEEPSEEK_API_KEY 或 PIPER_MODEL_PATH 没配；缺 AI 资产不影响开会，只影响 AI 功能 |
| 前端页面 404 / 刷新子路径 404 | nginx 没配好或没 reload；检查 sites-enabled 软链与 `nginx -t` |
