# Session Persistence Design

## Goal

Allow the claude-monitor server to restart without losing knowledge of existing sessions. On startup, previously-known sessions appear as "stopped" with their last terminal output preserved. Users can resume whichever sessions they want.

## Data Model

### `~/.config/claude-monitor/sessions.json`

Ordered JSON array of session metadata. Array order = spawn order.

```json
[
  {
    "name": "swift-fox",
    "dir": "/home/user/src/kura",
    "flags": ["--dangerously-skip-permissions", "--chrome", "--remote-control"],
    "started_at": "2026-03-24T10:00:00Z"
  }
]
```

### `~/.config/claude-monitor/sessions/{name}.raw`

Raw PTY output bytes (up to 256KB per session). Loaded into RawBuffer on startup so xterm.js can render the last screen with full color/formatting.

## Lifecycle

| Event | sessions.json | .raw file |
|-------|--------------|-----------|
| Spawn | Append entry | Created (empty) |
| Periodic tick (30s) | No change | Overwritten with current RawBuffer |
| Session stops (process exits) | No change | Flushed immediately |
| Kill | No change | Flushed immediately |
| Server shutdown (SIGTERM/SIGINT) | Already current | All running sessions flushed |
| Server startup | Read file | Load each .raw into RawBuffer |

## Startup Behavior

1. `loadSessions()` reads `sessions.json`
2. For each entry, create an Instance with status "stopped", ResumeID = name
3. Load `sessions/{name}.raw` into Instance's RawBuffer (if file exists)
4. Initialize empty RingBuffer and Broadcaster
5. Sessions appear in UI in saved order with Resume button

## Signal Handling

SIGTERM/SIGINT handler:
1. Flush all raw buffers to disk
2. Save sessions.json (already current)
3. Exit cleanly

## UI Changes

- Index page: order sessions by started_at (spawn order) instead of map iteration
- Session page: stopped sessions from previous runs render preserved raw output via xterm.js replay (existing mechanism)

## Out of Scope

- Deleting/archiving old sessions
- Auto-resuming on startup
- Re-attaching to orphaned processes
