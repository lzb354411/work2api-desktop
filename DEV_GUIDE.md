# Work2API Desktop 开发文档

> 适用版本：v1.4.0 ｜ 平台：Windows 10/11 ｜ 技术栈：Go 1.23 + Wails 2.10 + Vue 3 + systray

本文档面向二次开发者，描述项目分层架构、关键数据流、安全设计与扩展点。终端用户使用说明见 [README.md](./README.md)。

## 目录

1. [项目定位](#1-项目定位)
2. [目录结构](#2-目录结构)
3. [分层架构](#3-分层架构)
4. [核心数据流](#4-核心数据流)
5. [模块详解](#5-模块详解)
6. [安全设计](#6-安全设计)
7. [构建与调试](#7-构建与调试)
8. [扩展指南](#8-扩展指南)
9. [版本记录](#9-版本记录)

---

## 1. 项目定位

Work2API Desktop 是 [workbuddy2api](https://github.com/SliverKiss/workbuddy2api) 的 Windows 桌面单文件重构版，目标：

- **去掉 Bash / Docker / Python 依赖**：纯 Go + 前端单文件 exe 交付
- **图形化与系统托盘**：关闭窗口后台常驻，托盘快速打开/退出
- **安全加固**：DPAPI 加密凭据、强制 API Key、loopback 监听、日志脱敏
- **DeepSeek Harness 友好**：OpenAI 兼容接口可直接接入

## 2. 目录结构

```
work2api-desktop/
├── main.go                       # Wails 入口 + 系统托盘 + 单实例锁
├── api.go                        # Wails 前端绑定桥（DTO 组装，无业务）
├── wails.json                    # Wails 构建配置
├── go.mod / go.sum               # Go 依赖
├── build/
│   ├── appicon.ico               # 应用图标（嵌入二进制）
│   └── windows/                  # Windows 清单与图标资源
├── frontend/                     # Vue 3 + Vite 前端
│   ├── src/App.vue               # 单文件 UI（仪表盘/账号/设置/登录弹窗）
│   ├── src/main.js               # Vue 挂载入口
│   ├── src/style.css             # 全局样式
│   ├── vite.config.js
│   └── package.json
└── internal/                     # 业务代码（不可被外部 import）
    ├── app/                      # 应用核心
    │   ├── core.go               # 装配 store/pool/上游，管理 API 服务与后台调度
    │   ├── config.go             # Config + RingLog + DataDir + sealed 读写
    │   ├── autostart.go          # 开机自启动（HKCU Run 注册表开关）
    │   └── login.go              # 登录管理器（WB 设备流）
    ├── auth/                     # 账号凭据模型与加密存储
    │   ├── account.go            # Account/Snapshot + UID 白名单
    │   └── store.go              # accounts.dat 读写 + 旧版导入
    ├── pool/                     # 账号池（内存状态机 + 挑号策略）
    │   └── pool.go
    ├── secrets/                  # 静态加密（Windows DPAPI / 其他平台 base64）
    │   ├── secrets.go            # magic 头 + Seal/Open/SealFile/OpenFile
    │   ├── dpapi_windows.go      # CryptProtectData / CryptUnprotectData
    │   └── dpapi_fallback.go     # 非 Windows 退化实现
    ├── server/                   # OpenAI 兼容 HTTP 路由
    │   ├── server.go             # auth/models/chatCompletions/routeModel
    │   └── server_test.go        # 安全行为单测
    └── upstream/                # 上游客户端
        └── workbuddy/            # WorkBuddy（CodeBuddy CN / workbuddy.ai）
            ├── client.go         # Chat/Refresh/FetchModels/Checkin/UserResource
            ├── constants.go      # CN/Global 主机与默认模型
            ├── headers.go        # CommonHeaders/Chat/Billing/Refresh
            ├── payload.go        # 强制 stream + tool_choice 归一化
            ├── login.go          # 设备流 StartLogin/PollLogin
            └── sse.go            # 上游 OpenAI 兼容 SSE 的聚合与透传
```

## 3. 分层架构

自上而下五层，禁止反向依赖：

```
┌──────────────────────────────────────────────────────────────┐
│  Wails 桌面窗口 (main.go + api.go)                            │
│  - 系统托盘 / 单实例锁 / 关窗隐藏 / 日志推送 Events           │
└──────────────────────────────────────────────────────────────┘
                              │ Wails Bind
┌──────────────────────────────────────────────────────────────┐
│  internal/app (Core)                                          │
│  - 装配 Store/Pool/WB/RingLog                            │
│  - HTTP 服务生命周期（Start/Stop/RestartServer）             │
│  - 后台调度：tokenRefreshLoop / creditsLoop / checkinLoop     │
│  - 登录会话管理（loginMu 互斥，单并发）                       │
└──────────────────────────────────────────────────────────────┘
                              │
        ┌─────────────────────┼─────────────────────┐
        │                     │                     │
┌───────────────┐  ┌──────────────────┐  ┌────────────────┐
│ internal/auth  │  │ internal/pool    │  │ internal/server│
│ Account/Snap  │  │ 内存状态机       │  │ OpenAI HTTP    │
│ Store 加密落盘 │  │ Pick/Cooldown    │  │ 鉴权/路由/轮转 │
└───────────────┘  └──────────────────┘  └────────────────┘
        │                     │                     │
        └─────────────────────┼─────────────────────┘
                              │
        ┌─────────────────────┴─────────────────────┐
        │                                           │
┌────────────────────┐
│ upstream/workbuddy │
│ /v2/chat/completions│
│ OpenAI 兼容 SSE    │
└────────────────────┘

横切：internal/secrets（DPAPI 加密）被 auth/store 与 app/config 共用
```

**关键约束：**
- `api.go` 是 thin layer，仅组装 DTO 调用 `Core`，不含业务逻辑
- `server` 通过 `Deps` 闭包读取 Config（APIKey/DefaultProvider），避免循环依赖
- `upstream/*` 只依赖 `internal/auth` 的 `Snapshot`，不读 Pool 状态

## 4. 核心数据流

### 4.1 启动流程

```
main.main()
  ├─ app.NewCore()
  │   ├─ DataDir() → %APPDATA%\work2api-desktop (0700)
  │   ├─ LoadConfig(config.dat)         # 不存在则生成 APIKey 落盘
  │   ├─ auth.LoadStore(accounts.dat)
  │   ├─ NewRingLog(500)
  │   ├─ workbuddy.New               # 注入脱敏 log
  │   └─ Pool.Sync(accounts)            # 内存池对齐
  ├─ core.Start()
  │   ├─ startServerLocked(port)        # net.Listen("tcp","127.0.0.1:port")
  │   ├─ refreshAllCredits()            # 启动立即拉一次积分
  │   └─ go tokenRefreshLoop / creditsLoop / checkinLoop
  ├─ NewAPI(core)                       # Wails 绑定桥
  ├─ go systray.Run(...)                # 系统托盘菜单
  └─ wails.Run(options.App)            # 阻塞主循环
```

`OnStartup` 钩子订阅 `RingLog` 并通过 `runtime.EventsEmit(ctx, "log", e)` 推送到前端；`OnBeforeClose` 拦截关闭，改为 `WindowHide` 实现关窗后台常驻。

### 4.2 一次 chat 请求

```
Client POST /v1/chat/completions  (Authorization: Bearer <key>)
  │
  ▼
server.withAuth                          # secureEqualBearer 常数时间比较
  │  空 Key → 503 server_not_ready
  │  错 Key → 401 invalid_api_key
  ▼
chatCompletions
  ├─ io.LimitReader(8MB) → 超 413 payload_too_large
  ├─ peek model → routeModel()
  │     wb/x    → workbuddy + 剥前缀
  │     默认    → config.defaultProvider (auto 跨上游挑)
  ├─ 重写 body.model = cleanModel
  └─ for i in 0..maxRotate(3):
        acct = Pool.Pick(provider, tried)        # healthy 中 credits 最多者
        if acct == nil: break
        if acct.NeedsRefresh(10min):
            upstream.RefreshToken(acct)          # 失败 → Cooldown 10min 换号
            store.Save()
        chatViaWorkBuddy(acct, body, stream)
        # done=true 响应已写完直接 return
        # done=false 计入 tried 继续轮换
  ↓
all unavailable → 503 no_healthy_account
```

**轮换语义**：`chatVia*` 返回 `true` 表示响应已写完（无论成败），`false` 表示该账号不可用可换下一个（已计入冷却/禁用）。

### 4.3 Token 刷新（写路径）

```
tokenRefreshLoop (每 5min)
  └─ refreshAllTokens
      └─ for each account:
          upstream.RefreshTokenIfNeeded(a, skew=10min)
            ├─ a.Lock()                              # 写锁整段
            ├─ a.NeedsRefreshLocked(skew)?           # 持锁重查防 TOCTOU
            ├─ refreshLocked: POST /oauth/ExchangeToken
            ├─ 失败：不改写任何字段（原子性）
            └─ 成功：AccessToken/RefreshToken/ExpiresAt 整体更新
          失败 → Pool.Cooldown(10min, "token refresh failed")
      changed → Store.Save()                          # DPAPI 加密 + 原子 rename
```

`refreshLocked` 必须在持锁状态下完成网络调用 + 字段更新，确保读路径 `Snap()` 不会读到半成品。失败路径不修改字段，避免出现"刷新失败但 ExpiresAt 已被改"的不可用账号。

### 4.4 登录流程

**WorkBuddy（设备流）：**
```
StartLogin("workbuddy")
  └─ WB.StartLogin() → POST /v2/plugin/auth/state → {state, authUrl}
  └─ 5min 超时标记 error
PollLoginStatus()
  └─ WB.PollLogin(state)
       ├─ GET /v2/plugin/auth/token?state=   → code!=0 = ErrPending
       ├─ GET /v2/plugin/login/account?state= → uid/nickname
       └─ 返回 *auth.Account → Store.Upsert
```

`loginMu` 互斥保证同一时刻只有一个登录流程，防止会话串扰。

### 4.5 后台调度

| Loop              | 频率     | 行为                                            |
|-------------------|----------|------------------------------------------------|
| tokenRefreshLoop  | 5 min    | 临近过期（10min 内）才刷新，失败冷却 10min      |
| creditsLoop       | 30 min   | 拉取所有账号积分写入 Pool（仅内存）            |
| checkinLoop       | 配置时间 | 启用则每日定时签到，完成后刷新积分            |

积分仅存内存：避免与 `accounts.dat` 双写不一致，重启后由 `refreshAllCredits` 立即回填。

## 5. 模块详解

### 5.1 internal/app

**Core** 是装配中心与生命周期管理器：
- `Start()` 启动 HTTP 服务 + 三个后台 loop
- `Stop()` 优雅关闭（3s 超时 Shutdown + 关闭登录 listener）
- `RestartServer()` 端口变更后重启监听
- `UpdateConfig(nc)` 持锁更新 + 落盘 + 按需重启
- `RegenerateAPIKey()` 重新生成 32 字符 hex Key 并落盘

**Config** ([internal/app/config.go](internal/app/config.go)):
```go
type Config struct {
    Port             int      // 默认 8317
    APIKey           string   // 首次启动自动生成
    DefaultProvider  string   // workbuddy | auto
    CheckinEnabled   bool
    CheckinTime      string   // HH:MM
    StartMinimized   bool
    AutoStart        bool     // 开机自启动（HKCU Run）
    CreditFloor      int64    // 积分保留阈值（0 = 不限制）
    DisabledProviders []string // 被禁用的上游（UI 开关）
}
```
首次启动检测：空 Key 自动生成、非法端口修正为 8317。空模型名请求的默认模型由 server 层内置回退（`fallbackDefaultModels`，DeepSeek v4 flash 正式版）。

**RingLog** 固定 500 条内存环形缓冲，支持 `Subscribe()` 推送给前端 Events；慢消费者非阻塞丢弃。

### 5.2 internal/auth

**Account** 关键设计：可变字段（AccessToken/RefreshToken/ExpiresAt）由 `sync.RWMutex` 保护：
- 读路径必须走 `Snap()` 持读锁拷贝快照
- 写路径（token 刷新）通过 `Lock()/Unlock()` 持写锁整段执行
- `NeedsRefreshLocked` 必须在已持锁状态下调用

```go
type Snapshot struct { /* Account 的全部字段值拷贝 */ }
func (a *Account) Snap() Snapshot  // 持读锁拷贝
func (a *Account) Lock()/Unlock()  // 供 upstream 刷新持写锁
```

**UID 白名单**：`^[A-Za-z0-9_-]{1,64}$`，杜绝旧版 `login.sh` 的路径遍历风险。

**Store**：
- `LoadStore` 单条损坏不影响整体（跳过）
- `Upsert` 按 provider+uid 去重
- `Save` 持读锁导出 JSON → `secrets.SealFile`（DPAPI 加密 + 0600 原子 rename）
- `ImportLegacyFile` 兼容旧版 workbuddy2api 的明文 `auths/*.json`，导入后自动转为加密存储

### 5.3 internal/pool

**纯内存状态机**，桌面单进程语义下重启即重置。

| 状态字段      | 含义                                  |
|---------------|---------------------------------------|
| `credits`     | 后台调度回填，不落盘                  |
| `disabled`    | session_dead 永久禁用（需重登恢复）   |
| `until`       | 冷却截止时间                          |
| `errCount`    | 连续错误计数，达阈值自动冷却          |

**Pick 策略**：`healthy` 账号中 `credits` 最多者；`auto` 模式跨 provider 全局挑；`tried` 跳过已试过的键（单请求轮换）；`skip` 跳过被禁用上游（UI 开关）；积分保留阈值 `CreditFloor > 0` 时跳过 `credits <= floor` 的账号（余额到线暂停，回填/签到超过阈值自动恢复；`PickForRead` 只读路径不受限，供模型列表查询）。

**Sync** 用最新账号列表对齐池：新账号加入、消失的剔除，**已有账号保留 credits/cooling 状态**。

### 5.4 internal/secrets

**格式**：`magic = "W2AE1\x00"` + 密文

| 平台    | protect/unprotect 实现                              |
|---------|-----------------------------------------------------|
| Windows | `crypt32.dll` CryptProtectData/CryptUnprotectData（CRYPTPROTECT_UI_FORBIDDEN） |
| 其他    | base64（仅供跨平台编译与测试，非生产用途）           |

**SealFile** 流程：`Seal(plain)` → 写 `path.tmp`（0600）→ `os.Rename` 原子替换。崩溃时不会留下半写文件。

**OpenFile** 兼容识别旧格式：无 magic 头时尝试 base64 解码，便于迁移。

**DPAPI 安全属性**：密文绑定当前 Windows 用户 + 机器，复制到其他机器/用户无法解密——等价于 Chromium 保存密码的机制。

### 5.5 internal/server

路由表（Wails 之外的标准 net/http ServeMux，Go 1.22+ 路由语法）：

| Method+Path                  | 鉴权 | 行为                                          |
|------------------------------|------|-----------------------------------------------|
| `POST /v1/chat/completions`  | 是   | 8MB 限体 + 路由前缀剥离 + 3 次轮换           |
| `GET /v1/models`             | 是   | 静态回退 + 动态拉取（1h 缓存 + 5min 失败负缓存） |
| `GET /status`                | 是   | 返回所有账号 Status                          |
| `GET /healthz`               | 否   | 探活                                          |

**鉴权** ([internal/server/server.go#L99-L131](internal/server/server.go)):
- 空 Key → 503 `server_not_ready`（杜绝默认空鉴权）
- 缺/错 Key → 401 `invalid_api_key`
- `secureEqualBearer` 常数时间 XOR 比较，防时序侧信道
- 兼容 `?key=` query 参数（部分 harness 支持）

**模型前缀路由**：
```
wb/auto       → provider=workbuddy, model=auto
glm-5.2       → defaultProvider (auto 跨上游挑)
```

**错误分类冷却**（`applyUpstreamError`）：

| Class            | 行为                  |
|------------------|-----------------------|
| plan_limit/hard_credit | Cooldown 12h     |
| soft_rate/not_found    | Cooldown 60s     |
| session_dead           | Disable（永久）  |
| 其他                   | NoteError 累计   |

### 5.6 internal/upstream/workbuddy

**双区域**：CN (`copilot.tencent.com` / `www.codebuddy.cn`) vs Global (`www.workbuddy.ai`)，由 `account.Domain` 判定。

**安全红线** ([internal/upstream/workbuddy/headers.go](internal/upstream/workbuddy/headers.go))：
- `X-Refresh-Token` 仅出现在 `RefreshHeaders`（refresh 端点）
- `ChatHeaders` 绝不携带 `X-Refresh-Token`
- 缺省字段用 `X-No-*` 约定占位

**Classify** ([internal/upstream/workbuddy/client.go#L65-L103](internal/upstream/workbuddy/client.go))：
- HTTP 402 / 中英文积分不足关键词 → ErrHardCredit（长冷却）
- "Offline user session not found" / 12153 → ErrSessionDead（禁用）
- 429 → ErrSoftRate ｜ 404 → ErrNotFound ｜ 5xx → ErrServer

**SSE**：上游本身即 OpenAI 兼容格式，`Stream` 逐行透传 + flush，`Aggregate` 聚合 delta.content。

**FetchModels**：拉 `/console/enterprises/personal/models`，过滤出 `agents[].name == "cli"` 的模型交集。

### 5.7 frontend

单文件 [App.vue](frontend/src/App.vue)，无路由无状态库：
- **侧栏导航**：仪表盘 / 账号管理 / 设置 / 隐藏到托盘
- **仪表盘**：4 项统计 + DeepSeek Harness 接入卡片 + 脱敏日志流
- **账号管理**：登录按钮（WB）、一键全签、列表（签到/积分/恢复/删除）；签到返回真实结果（成功/已签到/未生效/领取失败，原始响应记入日志）
- **设置**：端口、默认上游、积分保留阈值、签到开关与时间、启动最小化、开机自启动（HKCU Run 注册表）
- **模型页**：WorkBuddy 模型列表（动态拉取 + 真实上下文窗口）、模型 ID 一键复制（带前缀可直填 Agent）、上游启用开关（关闭后不消耗其积分）；空模型名请求由 server 内置回退 DeepSeek v4 flash 正式版
- **登录弹窗**：5min 超时倒计时、轮询 PollLoginStatus（2.5s 间隔）

数据流：
- 调用 `window.go.main.API.<Method>()` 进入 Wails Bind → `api.go`
- 5s 轮询 `GetDashboard` + `ListAccounts`
- 日志通过 `window.runtime.EventsOn('log', cb)` 实时推送

## 6. 安全设计

相对原版的关键加固清单（详细对照表见 [README.md](README.md#安全加固相对原版)）：

| 风险              | 缓解措施                                                  | 代码位置 |
|-------------------|-----------------------------------------------------------|----------|
| 凭据明文落盘      | DPAPI 整体加密 `accounts.dat`/`config.dat`（W2AE1 头）   | `secrets/*` |
| 默认空鉴权        | 首次启动生成 32 字符 hex Key；空 Key 503 拒绝            | `app/config.go` `server/server.go` |
| 时序侧信道        | `secureEqualBearer` 常数时间 XOR 比较                    | `server/server.go` |
| 监听 0.0.0.0      | 仅 `net.Listen("tcp","127.0.0.1:port")`，无对外选项      | `app/core.go` |
| 命令注入/路径遍历 | 纯 Go，UID 白名单 `^[A-Za-z0-9_-]{1,64}$`                | `auth/account.go` |
| 数据竞争          | `Account.Snap()` 读锁快照 + 写锁整段刷新                  | `auth/account.go` `upstream/*/client.go` |
| SSE 挂死          | `idleWatchdog` 5min 空闲强制断流                         | `upstream/workbuddy/client.go` |
| 日志泄敏          | 仅记 uid/状态码/错误分类，不打印上游 body 原文            | `upstream/*/client.go` `app/core.go#desensitizeErr` |
| 请求体无限制      | `LimitReader(8MB)` + 413 拒绝                            | `server/server.go` |
| 多账号串会话      | 每次登录独立 cookie jar；`loginMu` 单并发             | `upstream/workbuddy/login.go` `app/login.go` |
| 原子性破坏        | `SealFile` tmp+rename；`refreshLocked` 失败不改字段      | `secrets/secrets.go` `upstream/*/client.go` |

## 7. 构建与调试

### 7.1 依赖

- Go 1.22+（go.mod 声明 1.23）
- Node.js 18+
- [Wails CLI](https://wails.io) v2.10+：`go install github.com/wailsapp/wails/v2/cmd/wails@latest`

### 7.2 构建

```powershell
# 单文件 exe（产出 build/bin/work2api-desktop.exe）
wails build

# 运行测试
go test ./...

# 开发模式（HMR 前后端）
wails dev
```

`wails build` 内部执行：
1. `cd frontend && npm install && npm run build` → `frontend/dist`
2. `go build` + `//go:embed all:frontend/dist` 嵌入资源
3. `//go:embed build/appicon.ico` 嵌入托盘图标
4. 链接为单文件 exe（约 12MB）

### 7.3 数据目录

```
%APPDATA%\work2api-desktop\
├── config.dat      # DPAPI 加密，W2AE1 头
└── accounts.dat    # DPAPI 加密，W2AE1 头
```
换机器/用户不可解密。测试时 `t.TempDir()` 生成临时目录，secrets 走非 Windows fallback 路径（base64）。

### 7.4 安全测试

[internal/server/server_test.go](internal/server/server_test.go) 覆盖：
- `/healthz` 无鉴权可达（200）
- 空 Key → 503 `server_not_ready`（覆盖 GET models + POST chat）
- 缺 Key → 401
- 错 Key → 401
- 正确 Key → 200（空账号池返回静态模型表）
- 无账号 POST chat → 502/503/429
- 超 8MB body → 413

### 7.5 调试技巧

- **看不到日志**：UI 仪表盘「日志（脱敏）」实时流；后端 `RingLog.Snapshot(200)` 可逆序取最近 200 条
- **端口冲突**：`RestartServer()` 已支持端口变更热重启，无需重启进程
- **账号无法登录**：检查 `loginSession.status/err`
- **SSE 卡死**：5min `idleWatchdog` 兜底，超时后 `rc.Close()` 触发读返回错误
- **凭据无法解密**：仅在生成它的同一 Windows 用户下可解密；换用户需重新登录

## 8. 扩展指南

### 8.1 新增上游 Provider

1. 在 `internal/auth/account.go` 增加 `ProviderXxx` 常量
2. 在 `internal/upstream/xxx/` 新建包：`client.go`/`constants.go`/`headers.go`/`payload.go`/`sse.go`/`login.go`，参考 `workbuddy` 包结构
3. `internal/app/core.go` 增加字段 `Xxx *xxx.Client`，在 `NewCore` 装配、`Start` 注入 server.Deps、`refreshAllTokens`/`fetchCredits`/`checkinOne` 增加 switch 分支
4. `internal/server/server.go` 在 `chatCompletions`/`providerModels` 增加 provider 分支，新增 `chatViaXxx`
5. `api.go` 增加对应登录按钮调用，前端 `App.vue` 加按钮
6. 在 `routeModel` 增加模型前缀路由（如 `xxx/`）

### 8.2 新增 HTTP 端点

在 [internal/server/server.go#New](internal/server/server.go) 的 `mux.HandleFunc` 注册，按需套 `h.withAuth`。注意保持错误响应 `writeOpenAIError` 格式以兼容 OpenAI SDK。

### 8.3 调整挑号策略

修改 [internal/pool/pool.go#Pick](internal/pool/pool.go) 的 `best` 选择逻辑。例如：
- 加权随机（避免单账号被持续选中）
- 优先最低延迟账号（需在 Status 增加延迟字段并周期测量）
- 故障转移优先级（按账号积分/健康度）

### 8.4 修改后台调度频率

修改 [internal/app/core.go](internal/app/core.go) 顶部常量：
```go
const (
    refreshSkew  = 10 * time.Minute  // 提前刷新窗口
    refreshEvery = 5 * time.Minute   // token 检查频率
    creditsEvery = 30 * time.Minute  // 积分刷新频率
    errCooldown  = 10 * time.Minute  // 刷新失败冷却

    // 签到领取瞬时繁忙(9074)时的重试：次数与间隔
    checkinClaimRetries   = 3
    checkinClaimRetryWait = 5 * time.Second
)
```

### 8.5 新增前端页面

[frontend/src/App.vue](frontend/src/App.vue) 的 `tab` ref 控制视图切换。新增：
1. 在 `<aside>` 加 `nav-item` 切换 `tab`
2. 在 `<template>` 加 `<main v-else-if="tab === 'xxx'">`
3. 如需新数据：在 `api.go` 增 `func (a *API) GetXxx() {...}` 暴露给前端

### 8.6 自定义加密后端

替换 [internal/secrets/dpapi_windows.go](internal/secrets/dpapi_windows.go) 的 `protect`/`unprotect` 实现（如改用 AES-GCM + 用户口令派生密钥），保持 `Seal`/`Open`/`SealFile`/`OpenFile` 函数签名不变即可无侵入替换。注意更新 `magic` 版本号以兼容旧文件识别。

---

## 9. 版本记录

### v1.4.0（2026-08-20）— 移除 TraeWork，仅保留 WorkBuddy

**变更摘要**：TRAE SOLO 相关功能实测不成功，本版彻底移除，统一为 WorkBuddy 单上游。
- 删除 `internal/upstream/trae/`（6 文件）与 `internal/checkin/webengine.go`
- 移除浏览器网页签到引擎及 `chromedp` 依赖
- 账号池 / 模型路由 / 登录 / 签到 / 令牌刷新仅保留 WorkBuddy；通用组件与 WorkBuddy 功能不受影响

**发布前版本号提升（独立提交，不含业务逻辑）**：
- `internal/app/config.go`：`const Version = "1.3.0"` → `"1.4.0"`
- `wails.json`：`info.productVersion` `"1.3.0"` → `"1.4.0"`

**相关提交（main 分支，线性）**：

| Commit    | 说明                                             |
|-----------|--------------------------------------------------|
| `5810a10` | refactor: remove all TraeWork support, keep WorkBuddy only（+154/-2499） |
| `6af3eb5` | build: bump version to v1.4.0（版本号提升提交，含上述 2 处改动） |

标签：`v1.4.0`（已推送到 origin，对应提交 `6af3eb5`）

**发布产物**：
- `work2api-desktop.exe`（12,209,664 B，约 12.2MB，Windows 10/11 单文件绿色版）
- Release：https://github.com/lzb354411/work2api-desktop/releases/tag/v1.4.0
- 直链：https://github.com/lzb354411/work2api-desktop/releases/download/v1.4.0/work2api-desktop.exe

---

## 附录：关键常量速查

| 常量             | 值                                   | 位置 |
|------------------|--------------------------------------|------|
| Version          | `1.4.0`                              | `app/config.go` |
| 默认端口         | `8317`                               | `app/config.go` |
| 默认签到时间     | `09:05`                              | `app/config.go` |
| API Key 长度     | 32 字符 hex（16 字节随机）           | `app/config.go#GenerateAPIKey` |
| RingLog 容量     | 500                                  | `app/core.go#NewCore` |
| maxRotate        | 3                                    | `server/server.go` |
| maxBodyBytes     | 8 MB                                 | `server/server.go` |
| hardCooldown     | 12h                                  | `server/server.go` |
| softCooldown     | 60s                                  | `server/server.go` |
| streamIdleTimeout| 5min                                 | `upstream/workbuddy/client.go` |
| 签到繁忙重试     | 3 次 × 5s                            | `app/core.go` |
| WB ClientUA      | `CLI/2.63.2 CodeBuddy/2.63.2`        | `upstream/workbuddy/constants.go` |
| secrets magic    | `W2AE1\x00`                          | `secrets/secrets.go` |
| loginTimeout     | 5min                                 | `app/login.go` |
