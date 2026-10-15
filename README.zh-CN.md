# Ai-Shell

[English](README.md) | **中文**

由 LLM 驱动的 Linux 远程运维桌面台。在本地管理 SSH 主机，用自然语言让 Agent 排查问题，也可以自己敲命令 —— 人敲的命令仅硬拒绝凭据读取与不可逆破坏、其余直接执行；Agent 提出的高危变更才需人工确认。

基于 [Wails v2](https://wails.io/)（Go + Vue 3）。

## 功能

- **主机管理** — 支持密码、私钥（可带 passphrase）、本地 `ssh-agent` 三种登录方式。密钥落在本机加密凭证库；主密钥优先托管系统钥匙串，不可用时降级为 `0600` 密钥文件（界面会如实显示当前保护等级）。
- **主机指纹校验** — 按地址固定 TOFU 指纹（`knownhosts`），可「忘记指纹」让下次连接重新信任。
- **Agent 会话** — 同一输入框双通道：
  - 自然语言（或以 `?` 开头）→ 走 LLM Agent 与工具
  - 像 shell 的一行 → 经硬拒绝过滤后写进常驻终端执行（不经过 LLM）
- **运维工具箱** — 除单次 `run_command` / 文件类工具外，Agent 可结构化采集诊断（`collect_diagnostics`、`journal_query`、`systemd_status`、`net_listen`）、对远端状态做快照与对比（`snapshot_state` / `diff_state`）、跑变更后校验（`verify_checks`：服务、端口、文件、本机 HTTP），以及通过文件备份回滚（`list_file_backups` / `restore_file_backup`）。
- **视觉附图** — 在 LLM 方案中勾选「支持视觉 / 图片输入」后，可在对话里粘贴或拖入截图（PNG / JPEG / WebP / GIF；最多 3 张，单张 ≤ 4 MiB）。图片只进入**当前轮**请求，不写会话历史、不进审计。发送框与消息气泡以文件名芯片展示，悬停才预览。
- **文件管理** — 在界面里浏览远端目录并上传 / 下载（单次传输上限 32 MiB，支持进度与取消）。
- **策略引擎**
  - 硬拒绝：登录凭据路径、应用凭据文件（`.env`、`credentials.json`）、整块环境变量导出、破坏性命令（`mkfs`、`dd of=/dev/*`、`rm -rf /`、`shutdown` 等）
  - 高危确认（仅 Agent/LLM 命令）：删/建/改权限、装包、启停服务、杀进程、Docker/Kubernetes 变更类操作等
  - LLM 模式：手动（条条批准）或只读命令白名单
  - Agent 出网工具（`fetch_webpage` / `search_web`）需策略里显式开启 `allowWeb`
- **交互终端** — 完整 PTY，可跑 `top` / `vim` / `htop` 等（**绕过策略**，需要真终端时用）。每条 `(主机, 会话)` 各有一条独立 shell，切回时复用，`cwd` 与运行中的进程都还在。
- **终端画面进上下文** — 后端维护 VT 屏幕模型跟踪 PTY 字节流，识别全屏 TUI 接管，在程序退出时把定格帧交给 LLM。快照是**瞬态上下文**：不进会话历史。
- **会话** — 按主机命名的对话上下文，加密落盘，支持压缩 / 清空。每条会话有三层记忆（`turns` → 确定性 `archived` 归档 → 可选的 LLM `deep` 深度压缩）。
- **上下文上限可调** — 单条会话的轮数、单条工具输出字节数、总字节数均可调，且每台主机可**逐项**覆盖其中任意一项。每套 LLM 方案还可配置 `contextWindow`（或留空按模型名自动推断），供界面 token 圆环使用。
- **跨主机防护** — 默认只允许 Agent 工具操作本会话所属的那台主机；跨主机执行需显式开启（防横向移动）。
- **多套 LLM 方案** — 可新增、编辑、切换、删除多套 OpenAI 兼容配置；「测试」会真发一次请求，并把失败翻译成能照着改的提示。视觉开关与上下文窗口按方案分别配置。
- **审计日志** — Agent 工具与人工 shell 决策的本机加密记录，带哈希链（`Verify` 可校验）、段轮转与明文 JSONL 导出。
- **输出治理** — 回传 LLM 前脱敏（PEM 私钥、云厂商/GitHub/Slack token、JWT、连接串口令、`KEY=value` 赋值）；不可信远端输出包裹与注入话术检测。
- **流式输出** — 逐字增量（含推理型模型的思考内容）攒批推送前端；网关不支持 SSE 时自动回退非流式。

## Agent 会话 vs 交互终端

| | Agent 会话 | 交互终端 |
|---|---|---|
| 通道 | 无 PTY 的 SSH `Exec` | PTY |
| 策略 | 有 | 无（人自己负责） |
| 输出 | 有界捕获（每路 1 MiB） | 持续流 |
| 适合 | 单次命令、LLM 诊断 | 一直刷的 `top` / 编辑器 / 需输密码的 sudo |

需要 TTY 的交互程序在 Agent 会话里会失败（设计如此）。可改用 `top -b -n 1` 这类批处理形式，或切到 **交互终端**。

Agent 会话里还有第三条路径：**你自己敲的** shell 命令只过硬拒绝（不可逆破坏与凭据读取，`EvaluateHuman`），其余以你的身份直接写进常驻终端执行、不审批；只有 Agent 提出的命令才需人工确认。未知第三方二进制会先做 `command -v` 探测，远端不存在时提示改用自然语言交给 Agent。

## 终端画面如何进入模型上下文

TUI 程序的输出是「持续重绘的屏幕」而不是一段文本 —— 直接喂给 LLM 等于喂乱码，完全不看又让模型对那块屏幕上发生了什么一无所知。Ai-Shell 分三步解决：

1. 后端为每条终端维护一份最小 VT100/xterm 屏幕模型，把 PTY 原始字节流喂给它。
2. shell integration（OSC 133）标出命令边界，备用屏幕激活标出全屏程序。当命令结束且全屏重绘过、备用屏幕释放、或 PTY 退出时，管线定格最后一帧。
3. 这一帧走完整安全管线（裁剪 → 脱敏 → 注入检测 → 审计）后作为**瞬态上下文**注入 —— 出现在下一次请求里，但**永不写入会话历史**，队列上限 4 帧。

## 环境要求

- Go 1.26+（与 `go.mod` 里的 `go` 指令一致）
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

### 预编译包

推送 `v*` 标签后，GitHub Actions 会打包 Windows / macOS 并发布到 [Releases](https://github.com/3thinkthendoit/Ai-Shell/releases)（见 `.github/workflows/release.yml`）：

```bash
git tag v0.1.0
git push origin v0.1.0
```

产物：`Ai-Shell-windows-amd64.zip`、`Ai-Shell-darwin-universal.zip`，以及 `SHA256SUMS.txt`。

### Windows 一键打包

命令行跑直接可用的脚本（不受 PowerShell Execution Policy 限制）：

```bat
scripts\build.bat          :: 只打包
scripts\build.bat run      :: 打包并启动
scripts\build.bat dev      :: 热重载开发
```

也可以双击 `scripts\build.bat`，或使用 PowerShell 版本：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
```

两个脚本都会自动补齐 `wails` CLI 与前端依赖，最终执行
`wails build -clean -trimpath -webview2 embed`。`-webview2 embed` 会内嵌
WebView2 bootstrapper，目标机没装 WebView2 也能运行。

产物在 `build/bin/`（已被 git 忽略），单文件绿色版，直接拷 `Ai-Shell.exe` 即可。

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
├── app.go / app_*.go     # Wails 绑定（主机、LLM、策略、shell、PTY、会话、文件）
├── main.go
├── wails.json
├── frontend/             # Vue 3 + Vite 界面
└── internal/
    ├── agent/            # LLM 工具循环、视觉附图、运维工具、会话记忆、压缩
    ├── audit/            # 加密、哈希链、可轮转的审计日志
    ├── llm/              # OpenAI 兼容 Chat 客户端（流式、多模态、回退）
    ├── policy/           # 硬拒绝 / 高危 / 白名单 / 脱敏 / 注入检测
    ├── sshclient/        # SSH Exec + PTY + VT 屏幕模型与快照器 + 文件传输
    ├── sshtest/          # 测试用假 SSH
    └── vault/            # 加密配置、主机密钥、LLM 方案、策略
```

## 配置

首次启动会在本机创建配置目录（路径随系统而定）—— 通常是 `{UserConfigDir}/Ai-Shell`（Windows 上如 `%AppData%\Ai-Shell`），否则回退到 `~/.ai-shell` 或可执行文件旁目录。界面会显示当前路径与密钥保护等级。请在应用内配置：

1. **LLM** — Base URL、模型、API Key（OpenAI 兼容接口）。每套方案可选手动配置**上下文窗口**，以及勾选「支持视觉 / 图片输入」。可保留多套方案并随时切换，下一次对话即生效，无需重启。
2. **主机** — 地址、用户、认证方式、标签与备注。
3. **策略** — 手动 / 白名单模式；编辑 Agent 用的只读命令库；可选开启 `allowWeb` 以启用网页抓取 / 网页搜索工具。
4. **上下文上限** — 会话轮数、单条工具输出字节数、总字节数。每台主机可**逐项**覆盖其中任意一项；留空即继承全局值。

全部状态（凭证库、会话、审计日志）共用同一把主密钥加密。无钥匙串守护进程的环境（headless Linux、CI）可设 `AISHELL_KEYFILE=1` 强制走密钥文件降级路径。

## 安全说明

- 仓库不放任何真实密钥；`.gitignore` 已排除凭证库与 `*.enc`。
- LLM 只能看到不透明的主机 ID —— 它永远读不到、猜不到、也拿不到密码、私钥或 passphrase。
- Agent / 工具路径不能读受保护路径（如 `/etc/shadow`、`~/.ssh`、`.env`）。
- 远端输出被视为不可信数据：先脱敏、限长，再用显式的 `<<<UNTRUSTED_REMOTE_OUTPUT>>>` 边界包裹；注入话术检测会把常见操纵短语标出来。系统提示要求模型**报告**疑似注入而不是照做。
- 你看到的和 LLM 看到的不是同一份 —— 人拿到原始输出，模型拿到净化后的内容。
- 用户附图会做 MIME + 魔数校验（拒绝 SVG）、体积上限，且**永不**写入会话历史或审计。
- 交互终端权限很大且不受策略约束 —— 当作普通 SSH 会话使用。
- 远端输出进 LLM 前不可全信；真正兜底仍是硬拒绝规则。

## 许可证

MIT — 见 [LICENSE](LICENSE)。
