# Session Persistence Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Persist session metadata and raw terminal output to disk so the server can restart without losing sessions.

**Architecture:** Add a `sessionOrder []string` slice alongside the existing `instances` map to track spawn order. Persist session metadata to `~/.config/claude-monitor/sessions.json` (ordered array) and raw PTY output to `~/.config/claude-monitor/sessions/{name}.raw`. On startup, load saved sessions as "stopped" instances with their raw output. Add signal handling for graceful shutdown and a 30s periodic flush.

**Tech Stack:** Go stdlib (`os/signal`, `encoding/json`, `os`). No new dependencies.

---

### Task 1: Add session order tracking and persistence functions

**Files:**
- Modify: `main.go:269-273` (global vars)
- Modify: `main.go:311-313` (configPath — add sibling helpers)

**Step 1: Add the sessionOrder slice and SessionRecord type**

Add after line 273 (after the `config` var block):

```go
var sessionOrder []string // tracks instance names in spawn order
```

Add a `SessionRecord` struct near the `Instance` struct (after line 111):

```go
type SessionRecord struct {
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	Flags     []string  `json:"flags"`
	StartedAt time.Time `json:"started_at"`
}
```

**Step 2: Add path helpers**

Add after `configPath()` (after line 313):

```go
func sessionsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-monitor", "sessions.json")
}

func sessionsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-monitor", "sessions")
}

func sessionRawPath(name string) string {
	return filepath.Join(sessionsDir(), name+".raw")
}
```

**Step 3: Add saveSessions function**

Caller must hold mu.RLock or mu.Lock.

```go
// saveSessions writes session metadata to disk. Caller must hold mu (read or write).
func saveSessions() {
	records := make([]SessionRecord, 0, len(sessionOrder))
	for _, name := range sessionOrder {
		inst := instances[name]
		records = append(records, SessionRecord{
			Name:      inst.Name,
			Dir:       inst.Dir,
			Flags:     inst.Flags,
			StartedAt: inst.StartedAt,
		})
	}

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		log.Printf("saveSessions marshal: %v", err)
		return
	}
	if err := os.WriteFile(sessionsFilePath(), data, 0644); err != nil {
		log.Printf("saveSessions write: %v", err)
	}
}
```

**Step 4: Add saveRawOutput function**

```go
// saveRawOutput writes an instance's raw PTY buffer to disk.
func saveRawOutput(name string, raw *RawBuffer) {
	os.MkdirAll(sessionsDir(), 0755)
	data := raw.Bytes()
	if err := os.WriteFile(sessionRawPath(name), data, 0644); err != nil {
		log.Printf("saveRawOutput %s: %v", name, err)
	}
}
```

**Step 5: Add loadSessions function**

```go
// loadSessions restores saved sessions as stopped instances on startup.
func loadSessions() {
	data, err := os.ReadFile(sessionsFilePath())
	if err != nil {
		return // no saved sessions, that's fine
	}

	var records []SessionRecord
	if err := json.Unmarshal(data, &records); err != nil {
		log.Printf("loadSessions: %v", err)
		return
	}

	for _, rec := range records {
		rawBuf := NewRawBuffer(256 * 1024)
		if raw, err := os.ReadFile(sessionRawPath(rec.Name)); err == nil {
			rawBuf.Write(raw)
		}

		inst := &Instance{
			Name:        rec.Name,
			Dir:         rec.Dir,
			Flags:       rec.Flags,
			Status:      "stopped",
			StartedAt:   rec.StartedAt,
			ResumeID:    rec.Name,
			output:      NewRingBuffer(500),
			rawOutput:   rawBuf,
			broadcaster: NewBroadcaster(),
		}
		instances[rec.Name] = inst
		sessionOrder = append(sessionOrder, rec.Name)
	}
	log.Printf("Loaded %d saved sessions", len(records))
}
```

**Step 6: Run tests**

Run: `cd /home/mutahhir/src/claude-monitor && go build ./...`
Expected: compiles cleanly

**Step 7: Commit**

```bash
git add main.go
git commit -m "feat: add session persistence data model and save/load functions"
```

---

### Task 2: Integrate persistence into session lifecycle

**Files:**
- Modify: `main.go` — `spawnInstance()` (line 332), process-exit goroutine (line 391), `handleSpawn()` (line 449)

**Step 1: Update spawnInstance to track order and save**

In `spawnInstance()`, after `instances[name] = inst` (line 372), add:

```go
	// Track spawn order (only for new sessions, not resumes of loaded ones)
	found := false
	for _, n := range sessionOrder {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		sessionOrder = append(sessionOrder, name)
	}
	saveSessions()
```

**Step 2: Flush raw output on process exit**

In the process-exit goroutine (line 391-398), after setting status to "stopped", add raw output flush:

```go
	go func() {
		cmd.Wait()
		mu.Lock()
		inst.Status = "stopped"
		inst.PID = 0
		inst.ResumeID = name
		mu.Unlock()
		saveRawOutput(name, inst.rawOutput)
	}()
```

Note: `saveRawOutput` does its own I/O and doesn't need the lock. Move `mu.Unlock()` before the save call so we don't hold the lock during disk I/O.

**Step 3: Build and verify**

Run: `cd /home/mutahhir/src/claude-monitor && go build ./...`
Expected: compiles cleanly

**Step 4: Commit**

```bash
git add main.go
git commit -m "feat: persist sessions on spawn and flush raw output on stop"
```

---

### Task 3: Add signal handling and periodic flush

**Files:**
- Modify: `main.go` — imports (add `"os/signal"`), `main()` function (line 275)

**Step 1: Add signal imports**

Add `"os/signal"` to the import block.

**Step 2: Add graceful shutdown in main()**

After `loadConfig()` (line 276), add:

```go
	loadSessions()

	// Graceful shutdown: flush all raw buffers on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Println("Shutting down, flushing session data...")
		mu.RLock()
		for _, name := range sessionOrder {
			if inst, ok := instances[name]; ok {
				saveRawOutput(name, inst.rawOutput)
			}
		}
		mu.RUnlock()
		os.Exit(0)
	}()

	// Periodic flush of raw output every 30s
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			mu.RLock()
			for _, name := range sessionOrder {
				if inst, ok := instances[name]; ok && inst.Status == "running" {
					saveRawOutput(name, inst.rawOutput)
				}
			}
			mu.RUnlock()
		}
	}()
```

**Step 3: Build and verify**

Run: `cd /home/mutahhir/src/claude-monitor && go build ./...`
Expected: compiles cleanly

**Step 4: Commit**

```bash
git add main.go
git commit -m "feat: add signal handling and periodic raw output flush"
```

---

### Task 4: Order sessions in the UI

**Files:**
- Modify: `main.go` — `handleIndex()` (line 433), `handleAPIInstances()` (line 599), index template (line 676)

**Step 1: Change handleIndex to pass ordered slice**

Replace `handleIndex` (lines 433-447):

```go
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	mu.RLock()
	defer mu.RUnlock()

	ordered := make([]*Instance, 0, len(sessionOrder))
	for _, name := range sessionOrder {
		if inst, ok := instances[name]; ok {
			ordered = append(ordered, inst)
		}
	}

	data := struct {
		Instances   []*Instance
		AllowedDirs []string
	}{ordered, allowedDirs}

	tmpl.Execute(w, data)
}
```

**Step 2: Change handleAPIInstances to return ordered array**

Replace `handleAPIInstances` (lines 599-604):

```go
func handleAPIInstances(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()

	ordered := make([]*Instance, 0, len(sessionOrder))
	for _, name := range sessionOrder {
		if inst, ok := instances[name]; ok {
			ordered = append(ordered, inst)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ordered)
}
```

**Step 3: Update the index template**

The template already uses `{{range .Instances}}` — since we changed Instances from `map[string]*Instance` to `[]*Instance`, the template range works the same way (iterating elements). No template changes needed.

**Step 4: Build and verify**

Run: `cd /home/mutahhir/src/claude-monitor && go build ./...`
Expected: compiles cleanly

**Step 5: Run existing tests**

Run: `cd /home/mutahhir/src/claude-monitor && go test ./...`
Expected: all tests pass (existing tests don't touch handlers)

**Step 6: Commit**

```bash
git add main.go
git commit -m "feat: order sessions by spawn order in UI and API"
```

---

### Task 5: Add tests for persistence functions

**Files:**
- Modify: `main_test.go`

**Step 1: Write test for save/load round-trip**

```go
func TestSaveLoadSessions(t *testing.T) {
	// Use a temp dir for config
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Create config dir
	os.MkdirAll(filepath.Join(tmpDir, ".config", "claude-monitor", "sessions"), 0755)

	// Set up test state
	instances = make(map[string]*Instance)
	sessionOrder = nil

	rawBuf := NewRawBuffer(256 * 1024)
	rawBuf.Write([]byte("\x1b[32mhello world\x1b[0m"))

	instances["test-fox"] = &Instance{
		Name:      "test-fox",
		Dir:       "/tmp/test",
		Flags:     []string{"--flag1"},
		Status:    "stopped",
		StartedAt: time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		ResumeID:  "test-fox",
		output:    NewRingBuffer(500),
		rawOutput: rawBuf,
		broadcaster: NewBroadcaster(),
	}
	sessionOrder = []string{"test-fox"}

	// Save
	saveSessions()
	saveRawOutput("test-fox", rawBuf)

	// Clear and reload
	instances = make(map[string]*Instance)
	sessionOrder = nil

	loadSessions()

	if len(sessionOrder) != 1 || sessionOrder[0] != "test-fox" {
		t.Fatalf("sessionOrder = %v, want [test-fox]", sessionOrder)
	}

	inst := instances["test-fox"]
	if inst == nil {
		t.Fatal("instance test-fox not loaded")
	}
	if inst.Status != "stopped" {
		t.Errorf("status = %q, want stopped", inst.Status)
	}
	if inst.Dir != "/tmp/test" {
		t.Errorf("dir = %q, want /tmp/test", inst.Dir)
	}

	// Verify raw output was loaded
	raw := inst.rawOutput.Bytes()
	if len(raw) == 0 {
		t.Error("raw output not loaded")
	}
	if string(raw) != "\x1b[32mhello world\x1b[0m" {
		t.Errorf("raw output = %q, want ANSI hello world", string(raw))
	}
}
```

**Step 2: Run tests**

Run: `cd /home/mutahhir/src/claude-monitor && go test ./... -v -run TestSaveLoad`
Expected: PASS

**Step 3: Commit**

```bash
git add main_test.go
git commit -m "test: add save/load sessions round-trip test"
```

---

### Task 6: Manual smoke test

**Step 1: Build the binary**

Run: `cd /home/mutahhir/src/claude-monitor && go build -o claude-monitor`

**Step 2: Verify the sessions directory and file structure**

Start the server, spawn a session via the UI, then Ctrl-C the server. Check:
- `~/.config/claude-monitor/sessions.json` exists with the session entry
- `~/.config/claude-monitor/sessions/{name}.raw` exists

**Step 3: Restart the server**

Start again. Verify:
- The previously spawned session appears as "stopped" in the UI
- Clicking into the session shows the preserved terminal output
- Clicking Resume spawns a new `claude -r {name}` process
