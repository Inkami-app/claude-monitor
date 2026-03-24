> **Built by [Inkami](https://inkami.app)** — a second brain you actually own. Think, plan, write, publish — stored where you choose, accessible everywhere.

# claude-monitor

A web dashboard for spawning, monitoring, and managing multiple Claude Code sessions from any browser.

## Motivation

I'm a full-time dev that's building [Inkami](https://inkami.app) in my spare time. This means that when I'm running multiple Claude Code instances and running chores, I'd like to make sure Claude is doing the right thing. Alas, as cool as Claude Code's remote-control is, it's horribly unreliable, and I ended up being far away from my desktop and stuck without access to the claude running on my desktop.

So, I built this small helper server. Start it up, and spawn any Claude Code instances with remote control. Each time it gets stuck, you just restart it. If you don't know if Claude is stuck or working on something, just go see the terminal output for any session.

Easy.

## Features

- **Multi-session management** -- spawn, name, and stop independent Claude sessions
- **Real terminal** via xterm.js with full PTY support
- **Session persistence** across server restarts
- **First-run setup wizard** powered by Bubble Tea
- **Optional authentication** with token-based login
- **Configurable directories** and Claude CLI flags
- **Mobile-friendly** responsive UI

## Install

```
go install github.com/inkami-app/claude-monitor@latest
```

Or build from source:

```
git clone https://github.com/inkami-app/claude-monitor.git
cd claude-monitor
go build -o claude-monitor .
```

## Quick start

Run `claude-monitor` -- the first run launches an interactive setup wizard. Then starts a server on the port of your choice. I use it with my [Tasilscale](https://tailscale.com/) setup, so I've made it easy to set up. If you're also using Tailscale, the startup wizard will detect it and install the correct HTTPS certs automatically.

## Configuration

Config file location: `~/.config/claude-monitor/config.json`

Example config:

```json
{
  "port": 7777,
  "cert_file": "",
  "key_file": "",
  "auth_token": "",
  "allowed_dirs": ["~", "~/projects"],
  "claude_flags": ["--dangerously-skip-permissions"]
}
```

### CLI flags

| Flag | Config key | Description | Default |
|------|-----------|-------------|---------|
| `--port` | `port` | HTTP port | `7777` |
| `--cert-file` | `cert_file` | TLS certificate | - |
| `--key-file` | `key_file` | TLS private key | - |
| `--auth-token` | `auth_token` | Authentication token | - |
| `--dir` | `allowed_dirs` | Allowed directory (repeatable) | `~` |
| `--claude-flag` | `claude_flags` | Claude CLI flag (repeatable) | - |

CLI flags override config file values.

## Authentication

- Set via config file (`auth_token`) or the `--auth-token` flag
- When set, all routes require login
- Supports cookie-based login and `Authorization: Bearer <token>` header
- When not set, no auth is required (suitable for localhost or Tailscale)

## Tailscale setup

### Accessing on your tailnet

```
tailscale serve --bg 7777
```

Then access via `https://<hostname>.<tailnet>.ts.net`.

### Using Tailscale TLS certs

```
tailscale cert <hostname>.<tailnet>.ts.net
```

Then configure `cert_file` and `key_file` in your config to point to the generated certificate and key.

### Public access via Funnel

```
tailscale funnel --bg 7777
```

Consider enabling authentication if using Funnel, since it exposes the UI to the public internet.

## License

MIT
