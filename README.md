# 星河启智 反代（xingheqizhi2api / sais2api）— OpenAI 兼容 API

**星河启智 反代** · 星河启智 API · 大圣科学智能 Agent 逆向 · ai4s 反代 ·
`aistudio.ai4s.com.cn` OpenAI 兼容 · 上海科学智能研究院（SAIS）对话接口 · `pi-agent` 协议

把「星河启智」（`aistudio.ai4s.com.cn`，大圣科学智能 Agent）网页对话背后的 `pi-agent` 接口，
逆向成本地一个 **OpenAI 兼容** HTTP 反代：任意 OpenAI SDK / `curl` 指向本服务即可反向调用
你自己账号的对话额度，响应支持非流式与流式（SSE）。

- **上游**：`https://aistudio.ai4s.com.cn/partner/pi-agent/api`（使用你自己账号登录后的登录态）。
- **用途**：**仅个人学习与自用**。本项目与上海科学智能研究院（SAIS）**没有任何关系**，非官方、未获授权。
  请遵守上游服务条款，不要用于商业用途、批量注册、转售或任何滥用行为；由此产生的一切后果由使用者自负。

> 本仓库只包含反代程序与部署文件，**不包含**任何账号、Cookie、token 或密钥。

`xingheqizhi2api` (sais2api) is an OpenAI-compatible reverse proxy for the
AI-for-Science chat agent at `aistudio.ai4s.com.cn`, with a built-in web admin panel
and automatic cookie-based credential capture.


---

## 目录

- [快速开始](#快速开始)
- [配置项说明](#配置项说明)
- [凭证获取](#凭证获取)
- [HTTP 端点](#http-端点)
- [管理面板](#管理面板)
- [速率测试](#速率测试)
- [协议要点](#协议要点)
- [Docker 部署](#docker-部署)
- [限制与排障](#限制与排障)
- [实测数据](docs/BENCHMARK.md)（TPS / 思考强度 / 上下文 320k / 输出长度 / 鹈鹕骑自行车）


---

## 快速开始

### 1. 准备配置

```bash
cp config.example.json config.json
```

把 `config.json` 里的 `api_key` 改成你自己的随机字符串，并把 `upstream.project_id` 填上：

> 打开网页对话页，地址栏形如
> `…/partner/scientific/coding?pid=1234567890123456789&tid=…`，
> `pid` 的值就是 `upstream.project_id`。

### 2. 编译

```bash
cd src
go build -trimpath -ldflags "-s -w" -o sais2api .
```

需要 Go 1.22+（仅用标准库，无第三方依赖）。

### 3. 启动

```bash
./sais2api -config ../config.json
```

默认监听 `127.0.0.1:3067`，浏览器打开 `http://127.0.0.1:3067/admin` 进入管理面板。

### 4. 接入客户端

```bash
export OPENAI_BASE_URL=http://127.0.0.1:3067/v1
export OPENAI_API_KEY=<config.json 里的 api_key>

curl -s $OPENAI_BASE_URL/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}]}'
```

---

## 配置项说明

| 键 | 说明 |
| --- | --- |
| `listen` | 监听地址，默认 `127.0.0.1:3067`。只监听本机，需要局域网访问自行改成 `0.0.0.0:3067` |
| `api_key` | 调用 `/v1/*` 时校验的密钥（`Authorization: Bearer` 或 `X-Api-Key`） |
| `admin_key` | 管理面板密钥；留空则首次启动自动生成并写回 `config.json` |
| `default_model` | 默认模型；`auto` 表示取上游模型列表的第一个 |
| `timeout_seconds` | 单次对话最长等待秒数，默认 900；设为 `0` 表示**无限等待**（不推荐在日常使用，上游无响应会一直占住并发槽） |
| `auto_capture` | 是否启动时自动抓取登录凭证，并每 10 分钟同步一次 |
| `cookie_sources` | 浏览器 Cookie 库路径列表（按顺序尝试，见下节） |
| `upstream.base` | `pi-agent` 接口根地址，一般不用改 |
| `upstream.cookie` | 登录态，格式 `Authorization=<JWT>`，由抓取功能自动写入 |
| `upstream.project_id` | **必填**，对话页 URL 里的 `pid` |
| `upstream.user_agent` | 上游请求使用的浏览器 UA（上游 WAF 会拦截非浏览器 UA） |
| `upstream.user_id` / `user_name` / `source` | 抓取时自动补全，用于面板展示与请求头 |

`config.json` 是严格 JSON（不支持注释）。写错键名不会报错但会被忽略，建议对着示例逐项核对。

---

## 凭证获取

上游不认任何 `sk_` 类 API Key，真实凭证是 **HttpOnly Cookie**，前端 JS 读不到，
所以本项目从**本机浏览器的 Cookie 数据库**里读。

### 方式一：自动抓取（默认，仅 macOS）

1. 用任一基于 Chromium 的浏览器（Chrome / Edge / Trae 内置浏览器等）登录
   `https://aistudio.ai4s.com.cn`；
2. 在管理面板点「抓取凭证」。

服务端做的事：把 `cookie_sources` 里的 SQLite 库复制到临时目录 → 执行

```sql
SELECT value FROM cookies
WHERE name='Authorization' AND host_key LIKE '%ai4s%'
ORDER BY expires_utc DESC LIMIT 1;
```

→ 解 JWT 取 `X-User-Id` / `X-User-Name` / `exp` → 用 `/models/search` 校验上游是否接受
→ 通过后写回 `config.json`。

`cookie_sources` 默认为 Trae 内置浏览器的路径；用 Chrome 的话可改成：

```
~/Library/Application Support/Google/Chrome/Default/Cookies
```

> 需要系统自带 `sqlite3`（macOS 自带）。首次读取可能被系统隐私保护拦截，
> 给终端/服务进程「完全磁盘访问权限」即可。

### 方式二：手工填

在浏览器 DevTools → Application → Cookies 里找到 `ai4s.com.cn` 域下名为 `Authorization`
的 Cookie，把它的值（一段 `eyJ…` 的 JWT）写进配置：

```json
"upstream": {
  "cookie": "Authorization=eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...",
  "project_id": "你的 pid"
}
```

JWT 自带 `exp`，面板会显示到期时间；过期后在浏览器重新登录一次、再抓取即可。

---

## HTTP 端点

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/healthz` | 无 | 健康检查，返回 `{"status":"ok"}` |
| GET | `/v1/models` | `api_key` | 模型列表（OpenAI 格式） |
| POST | `/v1/chat/completions` | `api_key` | 对话，支持 `stream:true` |
| GET | `/admin` | 页面本身不校验 | 管理面板（密钥由前端存 localStorage） |
| POST | `/admin/api/status` | `X-Admin-Key` | 账户 / 模型 / 统计 / 事件 |
| POST | `/admin/api/capture` | `X-Admin-Key` | 立即抓取凭证 |
| POST | `/admin/api/open-login` | `X-Admin-Key` | 打开登录页 |
| POST | `/admin/api/test` | `X-Admin-Key` | 发一条测试对话 |
| POST | `/admin/api/speed` | `X-Admin-Key` | 速率测试，body `{"model":"…"}` |

`/v1/chat/completions` 只转发文本：多轮 `messages` 会被压平成一条 prompt 发给上游
（system 在前，历史段落标注 `[对话历史]` / `[当前问题]`），因此 `usage` 字段为空。

---

## 管理面板

`http://127.0.0.1:3067/admin?key=<admin_key>`（首次带 `?key=` 打开后，密钥会存进 localStorage）。

面板包含：

- **账户信息**：登录状态、用户名、用户 ID、角色、凭证来源、到期倒计时
- **接入信息**：Base URL、API Key（一键复制）、默认模型、请求统计
- **可用模型**：模型 chips
- **最近事件**：环形日志（最近 200 条，前端展示 40 条）
- **速率测试**：见下节

---

## 速率测试

面板底部选模型 → 「开始测速」，会发一段固定中文写作题（约 200 字），然后报告粗略 TPS。

口径：

- 耗时取上游 `content[].startedAt → endedAt`（毫秒）的真实生成窗口，
  排除排队与轮询误差；排队时间单独列出；
- token 数为**估算**：中日韩字符 ×0.7 / 字，其余 4 字符 ≈ 1 token
  （上游不返回 usage，所以只能估算）；
- 每次只发一条请求，不并发。

接口：`POST /admin/api/speed`，body `{"model":"<模型 id>"}`，返回

```json
{
  "model": "…", "chars": 275, "tokens": 188,
  "gen_seconds": 2.75, "wall_seconds": 6.69, "queue_ms": 2002,
  "tps": 68.4, "preview": "…"
}
```

---

## 协议要点

逆向记录，方便后续维护：

1. `POST /sessions`，body `{"projectId","session_id","model"}`。
   `session_id` 为客户端生成的纯数字串（本项目用 18 位随机数），必须与 `projectId` 同时给出，
   否则上游报 `code 1002 projectId and session_id must be safe path segments`。
2. `POST /sessions/prompt`，body `{"message","modelConfigId","projectId","session_id"}`，
   返回 `{"queued":true}`。
3. `GET /sessions/messages?projectId=&session_id=` 轮询取结果：
   `data.messages[]` 末尾的 `assistant` 消息里，`content[]` 中 `type=="text"` 才是正文，
   `type=="thinking"` 是思考链（本项目丢弃）。
4. `GET /sessions/events` 虽然是 SSE，但只推 `connected` / `agent_end`，没有增量文本，
   所以采用轮询 `messages`。
5. 完成判定：`stopReason` 可能长期为空，用 `content[].endedAt > 0` 兜底。
6. 鉴权：Cookie `Authorization=<JWT>`，`iss=sais2026`，payload 含
   `X-User-Id` / `X-User-Name` / `X-User-Role` / `X-Auth-Status` / `X-User-Type` / `exp`。
   上游请求需要浏览器 UA，否则 WAF 直接 404。

---

## Docker 部署

```bash
docker compose up -d --build
```

镜像只监听 `3067`，`config.json` 以只读方式挂载。

> 容器内无法读取宿主浏览器的 Cookie 库，因此容器部署请关闭 `auto_capture`，
> 用「方式二：手工填」把 `upstream.cookie` 和 `upstream.user_id` / `user_name` 写进配置。

---

## 限制与排障

| 现象 | 原因 / 处理 |
| --- | --- |
| `401 Jwt is missing` | `upstream.cookie` 为空或没带 `Authorization=` 前缀；重新抓取一次 |
| 上游 `404` | UA 不是浏览器 UA；检查 `upstream.user_agent` |
| `等待模型回复超时` | 上游变慢或模型不可用；调大 `timeout_seconds`，或换模型 |
| `/v1/models` 返回 502 | 凭证失效；面板看「最近事件」，重新登录后抓取 |
| 面板提示「缺少管理密钥」 | 访问 `/admin` 时带 `?key=<admin_key>` |
| 抓取报「没有找到可用的浏览器 Cookie 库」 | `cookie_sources` 路径不对，或服务进程没有磁盘访问权限 |

其他：

- 并发上限 4；槽满时**新请求抢占最旧的在途请求**——旧请求立刻收到
  `503 {"error":{"message":"该请求已被更新的请求抢占"}}`，新请求正常排队执行；
- 只转发文本，不转发图片 / 工具调用；
- 上游是「对话式」接口，每次请求都会新建一个会话，历史靠 prompt 拼接。

---

## 常见问题（也是常见搜索词）

**星河启智（`aistudio.ai4s.com.cn`）有官方 API 吗？**
平台没有公开的 OpenAI 兼容 API；网页对话走内部的 `pi-agent` 接口，本项目就是把这个接口转出来。

**「星河启智 反代」是什么？**
就是用你自己账号的登录态，把星河启智 / 大圣科学智能 Agent 的对话能力包装成本地 OpenAI 兼容接口，
供任意客户端（Cherry Studio、ChatBox、LobeChat、各类 SDK）直接调用。

**支持哪些模型？**
模型列表从上游 `/models/search` 自动发现，不用写死。实测可用的有
`deepseek-v4.1-flash`、`deepseek-v4-pro-0813`、`glm-5.2`、`qwen3.8-max`。

**会不会有封号风险？**
本服务只是复用你自己账号的登录态，请求频率与网页端一致；但仍在服务条款灰区，
请自行评估风险，不要高频、批量或商用。

**和 `xiaohuanxiong2api`、`trae2api` 的关系？**
同一套思路的不同上游实现：都是「登录态 → 本地 OpenAI 兼容反代 + Web 管理面板」的形态，
彼此独立部署、端口错开（3066 / 3067 / 7864）。

**上下文能有多长？**
实测 32 万汉字（约 20 万+ token）仍能准确召回埋在正中间的信息，反代侧不做截断。
测法是 needle 检索：把随机编号 `ZQ????-7K` 藏在长文本 50% 位置，让模型只回该编号。

**为什么响应结尾会出现 `--检测到内容违规--安全护栏`？**
那是**上游的审核截断**，不是本项目的逻辑。上游命中内容安全策略时会中断生成并追加这行标记，
且 `stopReason` 仍返回 `stop`，客户端只能靠这个尾巴判断。整段生成会被截在命中点，
重试或换个表述通常能绕过。

**一次能生成多长？**
没有 `finish_reason=length` 这类硬截断，但模型会自行控制篇幅：要求 6000 字时实际约 1100–1400 字，
要求 500 字时反而写到 1300+ 字，要求 2000 字时约 3600 字。想写长文请**按章节拆成多次请求**。
另外长任务容易撞到 `timeout_seconds`（默认已调大），超时会返回
`502 {"error":{"message":"等待模型回复超时"}}`。

---

## 声明

本项目仅供学习与个人自用，与上游服务提供方无任何关联。使用者应自行承担因使用本工具产生的
全部责任，包括但不限于账号风险、服务条款风险与数据合规风险。请勿用于任何商业或滥用场景。
