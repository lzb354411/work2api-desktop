# Work2API Desktop

TRAE SOLO 与 WorkBuddy（CodeBuddy CN）的 OpenAI 兼容本地代理 —— **Windows 10/11 桌面单文件版**。

基于 [traework2api](https://github.com/Sliverkiss/traework2api) 与 [workbuddy2api](https://github.com/SliverKiss/workbuddy2api) 重构：去掉 Bash / Docker / Python 依赖，合并为一个 Wails 桌面应用（Go + Vue 3），带图形界面与系统托盘，专为在 DeepSeek Harness 等 OpenAI 兼容客户端中调用、并在 Windows 上方便地管理多账号而设计。

## 功能

- **双上游账号池**：TRAE SOLO 与 WorkBuddy 账号统一管理，自动轮转、冷却、禁用与恢复
- **OpenAI 兼容 API**：`/v1/chat/completions`（SSE 流式）、`/v1/models`、`/status`
- **模型前缀路由**：`trae/xxx` 强制走 TRAE，`wb/xxx` 强制走 WorkBuddy，无前缀按默认策略（trae / workbuddy / auto 积分最多者优先）
- **模型管理**：界面内分别查看两上游各自的模型列表（动态拉取，真实上下文窗口与最大输出），模型 ID 一键复制可直接填入 Agent；客户端未指定模型时自动回退 DeepSeek v4 flash 正式版
- **上游启用开关**：「模型」页每个上游标题后带开关，关闭后不再消耗该上游积分（auto 路由自动跳过；显式前缀路由返回 403）
- **积分保留阈值**：可自定义数值，账号余额 ≤ 阈值时自动暂停使用（保留积分），签到/积分回填超过阈值后自动恢复；0 = 不限制
- **自动 Token 刷新**：临期自动续期，失败自动冷却换号
- **积分查询与签到**：每日自动签到（可配置时间）+ 账号管理页手动单签/一键全签；显示积分 = 权益包剩余额度 + 签到积分池之和；签到返回真实结果（成功/已签到/未生效/领取失败，9074 繁忙自动重试，原始响应记入日志便于排查）
- **开机自启动**：可选（写入 HKCU Run 注册表，无需管理员权限），在「设置」页开关
- **系统托盘**：关闭窗口隐藏到托盘，后台持续提供 API 服务
- **浏览器 OAuth 登录**：TRAE 本地回调自动捕获，WorkBuddy 设备流轮询；支持导入旧版明文 auth 文件（自动转为加密存储）

## 安全加固（相对原版）

| 风险 | 原版问题 | 本版处理 |
|---|---|---|
| 凭据明文落盘 | auths/*.json 明文存储 token | `accounts.dat` / `config.dat` 整体 **Windows DPAPI 加密**（绑定当前用户，原子写入，0600） |
| 默认空鉴权 | API Key 为空即可调用 | 首次启动自动生成 32 字符随机 Key；空 Key 直接 503 拒绝服务；常数时间比较防时序侧信道 |
| 监听 0.0.0.0 | 默认暴露局域网 | 仅监听 `127.0.0.1`，不提供对外监听选项 |
| 命令注入 / 路径遍历 | login.sh 拼接 UID、curl 管道 | 纯 Go 实现，无 shell 调用；UID 白名单 `^[A-Za-z0-9_-]{1,64}$` |
| 数据竞争 | 请求头无锁读 AccessToken | `Snapshot()` 读锁快照 + 写锁整段刷新 |
| SSE 挂死 | 上游停滞无超时 | 空闲看门狗（5 分钟）强制断流 |
| 日志泄敏 | 原始响应体进日志 | 日志仅含 UID / 状态码 / 错误类别，环形缓冲 500 条 |
| 请求体无限制 | 无上限 | 8MB 上限 + 413 拒绝 |

## 构建

依赖：Go 1.22+、Node.js 18+、[Wails CLI](https://wails.io) v2.10+。

```powershell
wails build          # 产出 build/bin/work2api-desktop.exe（单文件）
go test ./...        # 运行测试
```

## 使用

1. 启动 `work2api-desktop.exe`，在「账号管理」中分别登录 TRAE / WorkBuddy 账号
2. 仪表盘复制 **Base URL**（`http://127.0.0.1:8317/v1`）与 **API Key**
3. 客户端（DeepSeek Harness 等）填入：

```
Base URL: http://127.0.0.1:8317/v1
API Key:  <仪表盘生成>
Model:    trae/glm-5.2     # 强制 TRAE（在「模型」页复制完整模型名）
          wb/deepseek-v4-flash   # 强制 WorkBuddy
          （可省略）        # 未指定时回退 DeepSeek v4 flash 正式版
```

数据目录：`%APPDATA%\work2api-desktop`（配置与账号均为 DPAPI 加密文件，换机器/用户不可解密）。

## 免责声明

本项目仅供学习与个人研究，请遵守上游服务条款；因滥用造成的账号限制与本项目无关。

## 致谢

- [traework2api](https://github.com/Sliverkiss/traework2api)
- [workbuddy2api](https://github.com/SliverKiss/workbuddy2api)
- [Wails](https://github.com/wailsapp/wails) · [Vue 3](https://vuejs.org) · [systray](https://github.com/fyne-io/systray)
