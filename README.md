# easylive-api（服务端）

轻量级 WebRTC 实时音视频会议系统 —— 媒体面（SFU）+ 信令 + 业务接口，**单进程**部署。

| 领域 | 选型 |
|---|---|
| 媒体面 | Go + Pion WebRTC v4（单端口 UDP mux、RTP 只转发不转码不混流） |
| HTTP / 信令 | Fiber v2 + `gofiber/contrib/websocket`（沿用 egg-api 的写法） |
| 业务接口 | gqlgen（当前为占位 handler，见 `server/server.go` 的 `TODO(gqlgen)`） |
| 数据 | PostgreSQL（pgx）；**不引入 Redis**：会话与媒体状态驻留进程内存 |
| 开发环境 | Dev Container（`.devcontainer/`，约定与 `egg-api` / `titanium-line-api` 一致） |

## 快速开始

### 方式 A：Dev Container（推荐）

```bash
cp .env.example .env          # compose 的 env_file 必需；缺失会直接报错
# VS Code：命令面板 → Dev Containers: Reopen in Container
go run .                      # 容器内
```

### 方式 B：宿主机直接跑

```bash
cp .env.example .env
docker compose up -d          # 只起 postgres（媒体面不需要它）
go run .                      # 监听 :8080，媒体 UDP :7881
```

> `docker compose` 只在宿主机可用；devcontainer 内默认没有 docker CLI（容器内用 `make run/gen/test`）。

### 验证

```bash
curl localhost:8080/healthz   # 房间/参与者/轨道/WS 连接数/goroutine/内存
go test ./signaling/ -v -timeout 180s
```

## 目录与职责

```
main.go                 装配顺序：配置 → 数据库(可选) → Pion 引擎 → 房间管理器 → 信令 Hub → Fiber → 优雅关闭
config/                 环境变量读取（含默认值）与关键配置打印
protocol/               信令协议的**唯一事实源**（Go 侧），与 web/src/lib/protocol.ts 1:1 对齐
sfu/
  engine.go             Pion SettingEngine：单端口 UDP mux、对外地址广播(SetNAT1To1IPs)、STUN/TURN、默认拦截器
  room.go               Manager（房间表）+ Room（参与者/轨道/订阅矩阵、广播、状态快照）
  participant.go        两条 PeerConnection（pub/sub）、发布与订阅、ICE 候选、重协商
  track.go              PublishedTrack：RTP 转发热路径、关键帧请求(PLI)、RTCP 排空
signaling/
  hub.go                会话注册表、连接数上限
  client.go             单连接的读写泵（唯一写协程）、心跳、请求响应回带 id
  handler.go            /ws 路由 + 协议分发（请求→响应，状态变更→事件）
  e2e_test.go           端到端冒烟测试（不需要浏览器）
server/server.go        Fiber 装配：recover/logger/cors/limiter、/healthz、/ws、/graphql、优雅关闭
store/                  pgx 连接池 + 房间元数据读写（数据库不可用时降级，不影响开会）
migrations/001_init.sql 最小表结构
```

## 信令协议速览

请求（客户端 → 服务端）：`join`、`leave`、`publish`、`answer`、`candidate`、`subscribe`、`unsubscribe`、`unpublish`、`mute`、`chat`、`ping`
响应 / 事件（服务端 → 客户端）：`joined`、`publish_answer`、`subscribe_offer`、`participant_joined`、`participant_left`、`participant_state`、`track_published`、`track_unpublished`、`track_muted`、`active_speakers`、`hand_state`、`chat_message`、`pong`、`error`

约定：
- 消息是**扁平结构**，按 `type` 取字段；`id` 是幂等键，响应原样带回（客户端据此匹配）。
- `sessionId ≠ participantId`：前者绑定连接，后者绑定会议身份（为断线重连预留）。
- `publish` 可重复发送 —— 第二次即**重协商**（开始/停止屏幕共享走这条路）。
- **断线重连（M4）**：`joined` 会下发 `resumeToken`。信令连接意外断开时服务端**不立即视为离会**，
  而是把参与者标记为 `reconnecting`（广播 `participant_state`）并保留轨道与订阅，
  宽限期（`RESUME_GRACE`，默认 30s）内带 `resumeToken` 重连即可恢复**同一个 participantId**。
  两种恢复路径：
  - 媒体面还活着 → 只重建信令绑定（客户端对发布连接做 ICE restart，**SSRC 不变**，服务端不会重复建轨）；
  - 媒体面已死（网络切换/进程挂起）→ 客户端在 `join` 里置 `resetMedia=true`，
    服务端清掉旧轨道并广播 `track_unpublished`（**身份保留**），客户端用同一身份重新发布。
  主动 `leave` 不走宽限期，立即清理并作废令牌。令牌一次性，用过即失效。
- **活跃说话人**：服务端注册 RFC 6464 音频电平头扩展（`sdp.AudioLevelURI`），在转发热路径上
  解析每个音频包的 `-dBov` 值并做指数平滑，**不解码音频**；房间每 300ms 采样一次，
  **只在说话人集合变化时**广播 `active_speakers`（与 LiveKit 的 `ActiveSpeakersChanged` 同语义）。
- **数据通道（SCTP 数据面）**：客户端在 pub 连接上创建通道 `easylive`（客户端是该连接的 offerer），
  服务端在 `OnDataChannel` 里接收，并**在同一条 SCTP 关联上反向开一条** `easylive-s2c` 用于推送——
  DCEP OPEN 走带内信令，因此**不需要重新协商**（sub 连接上的 offer 流程很敏感，能不动就不动）。
  数据面上的消息与信令是**两套结构**（`protocol.DataMessage`）：

  | 数据面消息 | 服务端行为 |
  |---|---|
  | `reaction`（`{emoji}`） | **只中继给其他人**，不保存——瞬时事件，丢了无所谓 |
  | `raise_hand`（`{raised}`） | 记入房间状态并广播 `hand_state`——状态必须可靠、可恢复、新人可见 |

  设计原则：**瞬时事件走数据面（低延迟、可丢），状态变更走信令面（可靠、进快照）**。
  混用会让表情这类高频小消息挤占心跳与重连快照的队列。
  `DataMessage.FromID` 由服务端在中继时填充，客户端无法伪造他人身份。
- `unpublish` 用于停止发布某条轨道（如结束屏幕共享）。**发布者也会收到自己轨道的 `track_published`**，
  否则它不知道自己的服务端 trackId，也就无法撤销这条轨道（表现为别人永远看到最后一帧）。
- 轨道的生命周期只有两个出口：发布者主动 `unpublish`，或转发热路径结束（发布者断开等）时自动下线；
  两条路都会广播 `track_unpublished`，且**幂等**。
- `source` 字段声明**本次协商**新增媒体的来源（`camera`/`screen`）。
  浏览器不允许自定义 MediaStream id，所以无法只靠 SDP 区分摄像头与屏幕；
  更严谨的做法是自定义 RTP 头扩展（可作为后续工作写进论文）。

## 媒体面设计要点

1. **每个参与者两条 PeerConnection**：`pub`（客户端 offer / 服务端 answer）与 `sub`（服务端 offer / 客户端 answer）。
   职责分离后两端各自永远是固定角色，**彻底避免重复协商（glare）**，也便于独立重协商。
2. **单端口 UDP mux**：所有连接共用 `${MEDIA_UDP_PORT}`，防火墙/安全组只开一个 UDP 口。
   必须配 `SFU_ADVERTISE_IP`，否则容器/内网地址会被广播出去，浏览器连不上。
3. **按需转发**：一条轨道对应一个 `TrackLocalStaticRTP`，把它 `AddTrack` 到每个订阅者的 sub 连接，
   Pion 会把 `WriteRTP` 的包扇出给所有绑定；**没有订阅者时包直接丢弃**——这就是"轻量"的核心。
4. **关键帧**：新订阅者建立时会向发布者回 PLI，否则要等发布者下一个自然关键帧（表现为黑屏）。
5. **RTCP 排空**：`AddTrack` 后必须持续 `sender.Read()`，否则 Pion 内部管道会阻塞。

## 已验证（本次交付时实测）

| 项目 | 结果 |
|---|---|
| `gofmt -l .` | 无输出 |
| `go vet ./...` | 通过 |
| `go build ./...` | 通过 |
| `go test ./signaling/ -v` | 全部 PASS：信令链路、媒体转发（订阅端实收 RTP 包）、屏幕共享关键帧、轨道下线、断线恢复（含 `resetMedia`）、活跃说话人 |
| 运行期 | `/healthz` 返回 JSON 指标；数据库不可用时降级继续；`SIGTERM` 优雅退出正常 |

## 尚未实现（对应开发计划的后续里程碑）

- [ ] **断线重连**：会话快照恢复、`sessionId` 复用（M4）
- [ ] **gqlgen 接入**：`server/server.go` 里已留好 `adaptor.HTTPHandler` 的挂载点（M3）
- [ ] **GOP 缓存**：让新订阅者首帧更快；以及 NACK 合并/抑制（M6 优化项，也是论文的消融实验）
- [ ] **simulcast / 自适应码率**：当前每位发布者只有一路码流
- [ ] **鉴权**：房间口令 + coturn 临时凭证已就绪，但 `/ws` 尚未校验 token
- [ ] **多房间隔离与容量保护**：目前只有每房间人数上限与全局连接数上限
- [ ] **拥塞控制**：已启用 Pion 默认拦截器（NACK/Report/TWCC），但尚未在 SFU 侧终结订阅腿的带宽估计

## 常见坑

| 现象 | 原因 |
|---|---|
| 浏览器能连上信令但一直黑屏 | 没设 `SFU_ADVERTISE_IP`（广播了容器内网地址），或 UDP 端口没放通 |
| 局域网手机无法开摄像头 | 页面不是 HTTPS（`localhost` 例外） |
| compose 报 `env file .env not found` | 忘了 `cp .env.example .env` |
| 容器内 `docker compose` 不可用 | devcontainer 里没有 docker CLI，用宿主机的 `make up/logs` |
| 第二个 `publish` 报错 | 已支持重协商；若报错检查客户端是否在 `have-local-offer` 状态下重复发 offer |
