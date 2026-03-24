# Public Release Design

## Goal

Prepare claude-monitor for public release as an open-source repo. Four areas of work: configurable folders, authentication, UI redesign, and a README.

## 1. Configuration & CLI Flags

Config file at `~/.config/claude-monitor/config.json`:

```json
{
  "port": 7777,
  "cert_file": "",
  "key_file": "",
  "auth_token": "",
  "allowed_dirs": ["~/src", "~/projects"],
  "claude_flags": ["--dangerously-skip-permissions"]
}
```

Every field shadowable via CLI flag:

| Config key | CLI flag | Default |
|---|---|---|
| port | --port | 7777 |
| cert_file | --cert-file | "" |
| key_file | --key-file | "" |
| auth_token | --auth-token | "" |
| allowed_dirs | --dir (repeatable) | ["~"] |
| claude_flags | --claude-flag (repeatable) | [] |

CLI flags take precedence over config file. If no `allowed_dirs` specified anywhere, default to `~`. `claude_flags` are the flags passed to every `claude` CLI invocation (e.g. `--dangerously-skip-permissions`, `--chrome`). No hardcoded defaults — users choose what flags they want.

Implementation: replace hardcoded `allowedDirs` and hardcoded spawn flags. Use Go `flag` package. Parse flags first, load config file, merge (CLI wins).

## 1b. First-Run Setup Wizard

When no `~/.config/claude-monitor/config.json` exists and stdin is a terminal, launch a Bubble Tea interactive wizard that asks:

1. Which directories to allow (browse/type paths)
2. What Claude flags to use (multi-select from common options + custom input)
3. What port to listen on (default 7777)
4. Whether to set an auth token (optional, generates a random one if yes)
5. TLS cert/key paths (optional, explain Tailscale certs)

Writes the resulting config.json and then starts the server normally.

Dependencies: `github.com/charmbracelet/bubbletea`, `github.com/charmbracelet/lipgloss`, `github.com/charmbracelet/huh` (form library).

If stdin is not a terminal (e.g. running as a service), skip the wizard and use defaults.

## 2. Authentication

- If `auth_token` is set (config or `--auth-token`), all routes require auth.
- If not set, no auth (Tailscale/localhost users).
- Login flow: `GET /login` shows password form. `POST /login` validates token, sets secure cookie (`claude-monitor-auth`, HttpOnly, SameSite=Strict, Secure when TLS).
- All routes check cookie via middleware.
- API access: also accept `Authorization: Bearer <token>` header.
- `/login` page bypasses middleware.

## 3. UI: Add Folder from Dashboard

- Index page: input field + "Add" button below spawn grid.
- `POST /api/dirs` with `{"dir": "/path"}` — validates path exists on disk, adds to in-memory list and persists to config.json.
- `DELETE /api/dirs` with `{"dir": "..."}` — removes it.
- Spawn grid re-renders with new directory.

## 4. UI Redesign

Drop:
- Scanline overlay
- Left border accents on cards and spawn buttons
- Cyan glow/neon effects
- Tiny font sizes (0.55rem)

Keep:
- Dark background
- JetBrains Mono
- Status color coding (green/red/amber)

Add:
- More whitespace (larger padding, more section gaps)
- Larger base font sizes (minimum ~0.75rem)
- Cleaner card style: subtle background + border, no left accent
- Higher contrast text
- Cleaner pill-shaped badges

Direction: clean dark dashboard (Linear/GitHub dark mode feel), not sci-fi terminal.

## 5. README

Structure:
1. One-line description
2. Screenshot placeholder
3. Install (go install / build from source)
4. Quick start
5. Configuration (config.json reference + CLI flags table)
6. Authentication
7. Tailscale setup (tailscale serve / Tailscale certs)
8. License (MIT)

## 6. Tailscale

README-only guidance. Document how to:
- Use `tailscale serve` to expose claude-monitor on tailnet
- Use Tailscale TLS certs with `cert_file`/`key_file` config
- Use `tailscale funnel` for public access
