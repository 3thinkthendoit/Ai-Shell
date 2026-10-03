# Ai-Shell

**English** | [中文](README.zh-CN.md)

LLM-powered remote Linux operations console. Manage SSH hosts from a desktop app, ask an agent to diagnose problems in natural language, or run shell commands yourself — with a policy engine that blocks credential reads and dangerous actions, and asks for confirmation on high-risk changes.

Built with [Wails v2](https://wails.io/) (Go + Vue 3).

## Features

- **Host inventory** — SSH hosts with password, private key (optionally passphrased), or local `ssh-agent` auth. Secrets are stored in an encrypted local vault; the master key lives in the OS keychain when available and falls back to a `0600` key file otherwise (the UI always reports which level is in effect).
- **Host key verification** — TOFU fingerprints are pinned per address (via `knownhosts`); you can forget a fingerprint to re-trust on the next connection.
- **Agent session** — Dual input in one console:
  - Natural language (or a `?` prefix) → LLM agent with tools
  - Shell-like lines → direct remote `Exec` through the policy engine (no LLM)
- **Policy engine**
  - Hard deny: login/credential paths, app credential files (`.env`, `credentials.json`), full environment dumps, destructive ops (`mkfs`, `dd of=/dev/*`, `rm -rf /`, `shutdown`, …)
  - High-risk confirm: delete/create/chmod, package changes, service restarts, process kills, Docker/Kubernetes mutations, …
  - LLM modes: manual (approve every tool) or read-only command whitelist
- **Interactive terminal** — Full PTY for `top`, `vim`, `htop`, and other TTY programs (bypasses policy; use when you need a real terminal). Each `(host, session)` gets its own shell that is reused when you switch back, so your `cwd` and running processes stay put.
- **Terminal snapshots into context** — A backend VT screen model tracks the PTY stream, detects when a full-screen TUI takes over, and hands the LLM the frozen final frame when the program exits. Snapshots are transient context: they never enter the session history.
- **Sessions** — Per-host named chat sessions, encrypted on disk, with compact/clear controls. Each session keeps a three-layer memory (`turns` → deterministic `archived` summaries → optional LLM `deep` compaction).
- **Configurable context limits** — Per-session turn count, per-tool output bytes, and total bytes are all adjustable, and each host can override any of the three individually.
- **Cross-host guard** — Agent tool calls may only target the session's own host by default; cross-host execution is an explicit opt-in (defense against lateral movement).
- **Multiple LLM profiles** — Add, edit, switch, and delete several OpenAI-compatible configurations; "Test" sends a real request and translates failures into actionable advice.
- **Audit log** — Encrypted, tamper-evident local trail of agent tools and human shell decisions, with a hash chain that `Verify` can check, segment rotation, and plaintext JSONL export.
- **Output hygiene** — Redaction of secrets before LLM context (PEM keys, cloud/GitHub/Slack tokens, JWTs, connection-string passwords, `KEY=value` assignments); untrusted remote-output framing and injection heuristics.
- **Streaming** — Token-level deltas (including reasoning content) are coalesced and pushed to the UI, with automatic fallback to non-streaming when a gateway ignores the `stream` flag.

## Agent session vs interactive terminal

| | Agent session | Interactive terminal |
|---|---|---|
| Channel | Non-PTY SSH `Exec` | PTY |
| Policy | Yes | No (operator-owned) |
| Output | Bounded capture (1 MiB per stream) | Continuous stream |
| Best for | One-shot commands, LLM diagnosis | Live `top` / editors / sudo password prompts |

Interactive programs that need a TTY will fail in Agent session (by design). Use a batch form such as `top -b -n 1`, or switch to **Interactive terminal**.

A third path exists inside the Agent session: shell commands **you** type are checked by the policy engine too, but only high-risk ones require confirmation (`EvaluateHuman`). Unknown third-party binaries are probed with `command -v` first, and if they don't exist remotely the app suggests rephrasing the request for the Agent instead.

## How a terminal snapshot reaches the model

A TUI program's output is a continuously repainted screen, not a line of text — feeding it raw to the LLM produces garbage, but ignoring it leaves the model blind to what happened on screen. Ai-Shell resolves this in three steps:

1. The backend keeps a minimal VT100/xterm screen model per terminal and feeds it the raw PTY byte stream.
2. Shell integration (OSC 133) marks command boundaries, and alternate-screen activation marks full-screen programs. When a command finishes a full-screen redraw, the alternate screen is released, or the PTY exits, the pipeline freezes the last frame.
3. That frame goes through the full safety pipeline (clip → redact → injection scan → audit) before being injected as **transient context** — it appears in the next request but is never written to the session history, and the queue is capped at four frames.

## Requirements

- Go 1.26+ (matches the `go` directive in `go.mod`)
- Node.js 18+ and npm
- [Wails CLI](https://wails.io/docs/gettingstarted/installation) v2
- A desktop WebView2 / WebKit runtime as required by Wails on your OS

## Quick start

```bash
# Clone
git clone https://github.com/3thinkthendoit/Ai-Shell.git
cd Ai-Shell

# Frontend deps
cd frontend && npm install && cd ..

# Dev (hot reload)
wails dev

# Or production build
wails build
```

The binary is written under `build/bin/` (ignored by git).

## Tests

```bash
# Go
go test ./... -p 1

# Frontend
cd frontend && npm test
```

## Project layout

```
.
├── app.go / app_*.go     # Wails bindings (hosts, LLM, policy, shell, PTY, sessions)
├── main.go
├── wails.json
├── frontend/             # Vue 3 + Vite UI
└── internal/
    ├── agent/            # LLM tool loop, session memory, compaction, persistence
    ├── audit/            # Encrypted, hash-chained, rotating audit log
    ├── llm/              # OpenAI-compatible chat client (streaming + fallback)
    ├── policy/           # Deny / high-risk / whitelist / redaction / injection scan
    ├── sshclient/        # SSH Exec + PTY + VT screen model & snapshotter
    ├── sshtest/          # Fake SSH server for tests
    └── vault/            # Encrypted settings, host secrets, LLM profiles
```

## Configuration

On first launch the app creates a config directory (platform-specific) for the vault, sessions, and audit log. Configure:

1. **LLM** — Base URL, model, API key (OpenAI-compatible endpoints). You can keep several profiles and switch the active one at any time; the change takes effect on the next turn without a restart.
2. **Hosts** — Address, user, auth method, tags, and notes.
3. **Policy** — Manual vs whitelist mode; edit the read-only command library used by the agent.
4. **Context limits** — Session turns, stored tool-output bytes, and total bytes. Each host can override any of the three individually; leaving a field empty inherits the global value.

All state (vault, sessions, audit log) is encrypted with the same master key. Set `AISHELL_KEYFILE=1` to force the key-file fallback when no keychain daemon is available (headless Linux, CI).

## Security notes

- Credential and key material never belong in the repo; `.gitignore` excludes vault files and `*.enc`.
- The LLM only ever sees opaque host IDs — it can never read, guess, or receive a password, private key, or passphrase.
- Agent/tool paths cannot read protected paths (e.g. `/etc/shadow`, `~/.ssh`, `.env`).
- Remote output is treated as untrusted data: it is redacted, bounded, and wrapped in explicit `<<<UNTRUSTED_REMOTE_OUTPUT>>>` markers, and injection heuristics flag common manipulation phrases. The system prompt tells the model to report suspected injections rather than obey them.
- What you see is not what the LLM sees — you get the raw output, the model gets the sanitized payload.
- Interactive terminal is powerful and unrestricted — treat it like a normal SSH session.
- Soften trust in remote command output when feeding the LLM; hard denials remain the last line of defense.

## License

MIT — see [LICENSE](LICENSE).
