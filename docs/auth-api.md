# 登录与令牌接口契约（前端对接用）

> 这份文档是**前后端的唯一约定**。前端只需要这三条登录接口 + 一条令牌接口，
> 不需要、也不应该接触任何签名密钥。

## 0. 一句话流程

```
① 用户填用户名/密码 → POST /auth/login         （服务端下发 HttpOnly 会话 Cookie）
② 入会前            → POST /token {roomId}     （服务端按会话签发**房间级**令牌）
③ 用令牌连 WebSocket → ws /ws 消息 join{roomId, displayName, token}
④ 需要判断"我是谁/能不能当主持人" → GET /auth/me
⑤ 退出登录          → POST /auth/logout
```

**会话 Cookie 是 HttpOnly 的**：浏览器 JS 读不到，也不要试图读。
所有请求都必须带 Cookie —— 用 `fetch` 时加 `credentials: 'include'`
（同源直接调时可以省略，但显式写上更稳妥）。

---

## 1. POST /auth/login

请求：

```json
{ "username": "teacher", "password": "……" }
```

成功 `200`：

```json
{
  "user": { "id": 1, "username": "teacher", "displayName": "teacher", "role": "host" },
  "expiresAt": 1792175428240
}
```

同时 `Set-Cookie: easylive_session=…; HttpOnly; Path=/; SameSite=Lax`（生产环境带 `Secure`）。

失败：

| 状态 | 含义 | 界面建议 |
|---|---|---|
| `400` | `username`/`password` 为空或请求体不是 JSON | 提示补全 |
| `401` | 用户名或密码错误（**刻意不区分两者**，避免枚举账号） | 提示"用户名或密码错误" |
| `403` | 账号已停用 | 提示联系管理员 |
| `429` | 尝试过于频繁（同一 IP+用户名 每分钟 10 次） | 提示稍后再试 |
| `501` | 服务端未启用鉴权（`AUTH_MODE=off`）—— 此时**不需要登录**，直接入会 | 隐藏/跳过登录页 |
| `503` | 数据层不可用（未配数据库） | 提示服务暂不可用 |

## 2. GET /auth/me

已登录 `200`：`{ "user": { …同上… } }`；未登录 `401`：`{ "error": "未登录" }`。

用途：刷新页面后恢复登录态；据此决定"显示昵称还是登录按钮"。
**不要**用 `role` 在前端做安全判断——真正的判定在服务端，前端只用来决定界面。

## 3. POST /auth/logout

`204`，并清除 Cookie。

⚠️ 语义说明：这是**签名 Cookie** 方案，登出只是让浏览器丢弃 Cookie，
服务端无法单方面吊销某个会话（要支持"强制下线"需要引入会话表）。

## 4. POST /token

请求：`{ "roomId": "…" }`（已登录时 `identity`/`name`/`roomAdmin` **会被忽略**）

成功 `200`：

```json
{
  "token": "eyJ…",
  "identity": "teacher",
  "roomId": "r1",
  "expiresAt": 1792175428240,
  "video": { "roomJoin": true, "room": "r1", "canPublish": true, "canSubscribe": true,
             "canPublishData": true, "roomAdmin": true }
}
```

**主持人权限由服务端按账号角色判定**：`role=host` 才会得到 `roomAdmin: true`。
前端**不要**在请求体里传 `roomAdmin`（传了也不生效），更不要自己伪造权限位。

失败：`401`（无会话且无运维密钥）、`403`（服务端未配置密钥）、`501`（未启用鉴权，无需令牌）。
注意：`501` 是**正常状态**——此时前端应继续以"无令牌"方式入会，而不是报错。
把它和 `401/403` 区分开，是本项目踩过的坑（以前一律当成"无需令牌"，导致有鉴权时
用户进入一个没权限、也没有任何提示的会议）。

## 5. 运维/脚本路径（前端不要用）

`POST /token` 与 `GET /recordings`、`/graphql` 在非 development 环境下也接受
`X-API-Key: <TOKEN_API_KEY>`。这是给评测脚本与运维用的**服务端到服务端**凭据，
**绝不能**放进浏览器（那就等于把令牌签发权公开）。

## 6. 启用登录需要哪些环境变量

| 变量 | 说明 |
|---|---|
| `AUTH_MODE=required` | 打开鉴权（非开发环境未设置时会**默认按 required**） |
| `JWT_SECRET` | 令牌密钥；同时作为会话密钥的兜底（≥16 字符，否则**拒绝启动**） |
| `SESSION_SECRET` | 可选：单独指定会话签名密钥 |
| `SESSION_TTL` | 会话有效期，默认 `12h` |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | **引导第一个主持人账号**（仅当该用户名不存在时创建；绝不覆盖已有口令） |
| `DATABASE_URL` | 数据库（`users` 表在 `migrations/004_users.sql`） |

首次启动后即可用该账号登录；之后再要加人，直接在 `users` 表插一行
（`password_hash` 用 argon2id，`role` 取 `host` 或 `member`）。

---

## 7. 账号资料（左下角访客头像点开的那张 Dialog 需要这些）

所有接口都要求**已登录会话**（Cookie），未登录统一 `401 {"error":"未登录"}`。
**改资料和改密码都必须带上当前密码**——会话是可被劫持的凭据，不该能直接改名/改密。

### 7.1 PATCH /auth/profile

```json
{ "displayName": "李老师", "username": "teacher2", "currentPassword": "……" }
```
`displayName`、`username` 都可选（只传要改的）；`currentPassword` **必填**。

成功 `200`：`{ "user": { … } }`（结构与 `/auth/me` 一致，含 `avatarUrl`）

| 状态 | 含义 |
|---|---|
| `400` | 用户名长度/字符不合法（只允许字母数字与 `. _ -`，3–32 位）、昵称超 32 字、缺 `currentPassword` |
| `401` | 当前密码不正确 |
| `409` | 用户名已被占用 |
| `429` | 尝试过于频繁 |

### 7.2 POST /auth/password

```json
{ "currentPassword": "……", "newPassword": "……" }
```
成功 `204`。失败：`400`（新密码过短/与当前相同）、`401`（当前密码错）、`429`。

改完密码**旧密码立即失效**（已实测）。已签发的会话仍然有效（会话按用户 ID 签名）。

### 7.3 头像

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/auth/avatar` | `multipart/form-data`，字段名 **`file`**；只接受 `image/png`、`image/jpeg`、`image/webp`；上限 **512KB** |
| `DELETE` | `/auth/avatar` | 恢复默认头像，`204` |
| `GET` | `/auth/avatar/:id` | 取头像字节。**需要已登录会话**（未登录 `401`） |

上传成功 `200`：`{ "user": { …, "avatarUrl": "/auth/avatar/1?v=1792147936" } }`
失败：`400`（缺 `file`）、`413`（超过 512KB）、`415`（类型不支持）。

前端上传示例：

```ts
const form = new FormData()
form.append('file', file)                       // 不要手动设 Content-Type，浏览器会带 boundary
const res = await fetch('/auth/avatar', { method: 'POST', credentials: 'include', body: form })
```

`avatarUrl` 里带了 `?v=<更新时间>`：换头像后 URL 会变，浏览器不会用旧缓存。
`<img src={user.avatarUrl}>` 直接可用（同源、带 Cookie 即可）。

### 7.4 用户对象（`/auth/me`、登录、改资料、上传头像都返回它）

```json
{ "id": 1, "username": "teacher", "displayName": "李老师", "role": "host",
  "avatarUrl": "/auth/avatar/1?v=1792147936" }
```
`avatarUrl` **仅在设置过头像时出现**（没有该字段就显示默认头像：昵称首字母）。
**永远不含** `password_hash`，也不含头像字节。
