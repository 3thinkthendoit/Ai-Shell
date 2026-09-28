# Ai-Shell

[English](README.md) | **中文**

由 LLM 驱动的 Linux 远程运维桌面台。在本地管理 SSH 主机，用自然语言让 Agent 排查问题，也可以自己敲命令 —— 经策略引擎拦截凭据读取与破坏性操作，高危变更需人工确认。

基于 [Wails v2](https://wails.io/)（Go + Vue 3）。

## 功能

- **主机管理** — 支持密码 / 密钥登录；密钥落在本机加密凭证库（可用系统钥匙串）。
- **Agent 会话** — 同一输入框双通道：
  - 自然语言（或以 `?` 开头）→ 走 LLM Agent 与工具
  - 像 shell 的一行 → 经策略后直接远端 `Exec`（不经过 LLM）
- **策略引擎**
  - 硬拒绝：登录凭据路径、破坏性命令（`mkfs`、`rm -rf /` 等）
  - 高危确认：删/建/改权限、装包、启停服务等
  - LLM 模式：手动（条条批准）或只读命令白名单
- **交互终端** — 完整 PTY，可跑 `top` / `vim` / `htop` 等（**绕过策略**，需要真终端时用）。
- **会话** — 按主机命名的对话上下文，加密落盘，支持压缩 / 清空。
- **审计日志** — Agent 工具与人工 shell 决策的本机加密记录。
- **输出治理** — 回传 LLM 前脱敏；不可信远端输出包裹与注入话术检测。

## Agent 会话 vs 交互终端

| | Agent 会话 | 交互终端 |
|---|---|---|
| 通道 | 无 PTY 的 SSH `Exec` | PTY |
| 策略 | 有 | 无（人自己负责） |
| 适合 | 单次命令、LLM 诊断 | 一直刷的 `top` / 编辑器 / 需输密码的 sudo |

需要 TTY 的交互程序在 Agent 会话里会失败（设计如此）。可改用 `top -b -n 1` 这类批处理形式，或切到 **交互终端**。

## 环境要求

- Go 1.22+（模块目标为 Go 1.26）
- Node.js 18+ 与 npm
- [Wails CLI](https://wails.io/docs/gettingstarted/installation) v2
- 本机 WebView2 / WebKit（按 Wails 各平台要求）

## 快速开始

```bash
# 克隆
git clone https://github.com/3thinkthendoit/Ai-Shell.git
cd Ai-Shell

# 前端依赖
cd frontend && npm install && cd ..

# 开发（热重载）
wails dev

# 或打生产包
wails build
```

产物在 `build/bin/`（已被 git 忽略）。

## 测试

```bash
# Go
go test ./... -p 1

# 前端
cd frontend && npm test
```

## 目录结构

```
.
├── app.go / app_*.go     # Wails 绑定（主机、LLM、策略、shell、PTY、会话）
├── main.go
├── wails.json
├── frontend/             # Vue 3 + Vite 界面
└── internal/
    ├── agent/            # LLM 工具循环、会话、压缩
    ├── audit/            # 加密审计日志
    ├── llm/              # OpenAI 兼容 Chat 客户端
    ├── policy/           # 硬拒绝 / 高危 / 白名单
    ├── sshclient/        # SSH Exec + PTY
    ├── sshtest/          # 测试用假 SSH
    └── vault/            # 加密配置与主机密钥
```

## 配置

首次启动会在本机创建配置目录（路径随系统而定），用于凭证库、会话与审计。请在应用内配置：

1. **LLM** — Base URL、模型、API Key（OpenAI 兼容接口）。
2. **主机** — 地址、用户、认证方式。
3. **策略** — 手动 / 白名单模式；编辑 Agent 用的只读命令库。

## 安全说明

- 仓库不放任何真实密钥；`.gitignore` 已排除凭证库与 `*.enc`。
- Agent / 工具路径不能读受保护路径（如 `/etc/shadow`、`~/.ssh`、`.env`）。
- 交互终端权限很大且不受策略约束 —— 当作普通 SSH 会话使用。
- 远端输出进 LLM 前不可全信；真正兜底仍是硬拒绝规则。

## 许可证

MIT — 见 [LICENSE](LICENSE)。
