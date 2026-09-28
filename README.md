# Ai-Shell

**English** | [中文](README.zh-CN.md)

LLM-powered remote Linux operations console. Manage SSH hosts from a desktop app, ask an agent to diagnose problems in natural language, or run shell commands yourself — with a policy engine that blocks credential reads and dangerous actions, and asks for confirmation on high-risk changes.

Built with [Wails v2](https://wails.io/) (Go + Vue 3).

## Features

- **Host inventory** — SSH hosts with password or key auth; secrets stored in an encrypted local vault (OS keychain when available).
- **Agent session** — Dual input in one console:
  - Natural language (or a `?` prefix) → LLM agent with tools
  - Shell-like lines → direct remote `Exec` through the policy engine (no LLM)
- **Policy engine**
  - Hard deny: login/credential paths, destructive ops (`mkfs`, `rm -rf /`, …)
  - High-risk confirm: delete/create/chmod, package changes, service restarts, …
  - LLM modes: manual (approve every tool) or read-only command whitelist
- **Interactive terminal** — Full PTY for `top`, `vim`, `htop`, and other TTY programs (bypasses policy; use when you need a real terminal).
- **Sessions** — Per-host named chat sessions, encrypted on disk, with compact/clear controls.
- **Audit log** — Encrypted local trail of agent tools and human shell decisions.
- **Output hygiene** — Redaction of secrets before LLM context; untrusted remote-output framing and injection heuristics.

## Agent session vs interactive terminal

| | Agent session | Interactive terminal |
|---|---|---|
| Channel | Non-PTY SSH `Exec` | PTY |
| Policy | Yes | No (operator-owned) |
| Best for | One-shot commands, LLM diagnosis | Live `top` / editors / sudo password prompts |

Interactive programs that need a TTY will fail in Agent session (by design). Use a batch form such as `top -b -n 1`, or switch to **Interactive terminal**.

## Requirements

- Go 1.22+ (module targets Go 1.26)
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
    ├── agent/            # LLM tool loop, sessions, compaction
    ├── audit/            # Encrypted audit log
    ├── llm/              # OpenAI-compatible chat client
    ├── policy/           # Deny / high-risk / whitelist
    ├── sshclient/        # SSH Exec + PTY
    ├── sshtest/          # Fake SSH server for tests
    └── vault/            # Encrypted settings & host secrets
```

## Configuration

On first launch the app creates a config directory (platform-specific) for the vault, sessions, and audit log. Configure:

1. **LLM** — Base URL, model, API key (OpenAI-compatible endpoints).
2. **Hosts** — Address, user, auth method.
3. **Policy** — Manual vs whitelist mode; edit the read-only command library used by the agent.

## Security notes

- Credential and key material never belong in the repo; `.gitignore` excludes vault files and `*.enc`.
- Agent/tool paths cannot read protected paths (e.g. `/etc/shadow`, `~/.ssh`, `.env`).
- Interactive terminal is powerful and unrestricted — treat it like a normal SSH session.
- Soften trust in remote command output when feeding the LLM; hard denials remain the last line of defense.

## License

MIT — see [LICENSE](LICENSE).
