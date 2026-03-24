# Public Release Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Prepare claude-monitor for public release with configurable dirs, configurable Claude flags, first-run wizard, CLI flag overrides, auth, UI redesign, and README.

**Architecture:** Single-file Go app (`main.go`) plus a `setup.go` for the Bubble Tea first-run wizard. All config/CLI/auth changes are backend. UI is embedded Go templates (in `main.go`). README is a new file at repo root.

**Tech Stack:** Go stdlib (`flag`, `net/http`, `crypto/subtle`), existing deps (gorilla/websocket, creack/pty), new deps (charmbracelet/bubbletea, charmbracelet/huh, charmbracelet/lipgloss), xterm.js for terminal UI.

---

### Task 1: Config struct + CLI flags

Replace the hardcoded `allowedDirs` and expand `Config` struct to support all fields. Add CLI flag parsing with config-file fallback.

**Files:**
- Modify: `main.go:72-96` (remove hardcoded `allowedDirs`, `expandDir`, `isAllowedDir`)
- Modify: `main.go:121-125` (expand `Config` struct)
- Modify: `main.go:452-466` (rewrite `loadConfig`)
- Modify: `main.go:285-352` (update `main` to parse flags before config)

**Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestCLIOverridesConfig(t *testing.T) {
	// Save and restore global config
	origConfig := config
	defer func() { config = origConfig }()

	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.json")
	os.WriteFile(cfgFile, []byte(`{"port": 9999, "allowed_dirs": ["~/old"]}`), 0644)

	// Simulate CLI flags overriding config
	config = Config{}
	loadConfigFrom(cfgFile)
	if config.Port != 9999 {
		t.Errorf("port = %d, want 9999", config.Port)
	}
	if len(config.AllowedDirs) != 1 || config.AllowedDirs[0] != "~/old" {
		t.Errorf("allowed_dirs = %v, want [~/old]", config.AllowedDirs)
	}

	// CLI override
	applyCLIOverrides(8080, "", "", "", []string{"~/new"}, []string{"--verbose"})
	if config.Port != 8080 {
		t.Errorf("port after override = %d, want 8080", config.Port)
	}
	if len(config.AllowedDirs) != 1 || config.AllowedDirs[0] != "~/new" {
		t.Errorf("allowed_dirs after override = %v, want [~/new]", config.AllowedDirs)
	}
	if len(config.ClaudeFlags) != 1 || config.ClaudeFlags[0] != "--verbose" {
		t.Errorf("claude_flags after override = %v, want [--verbose]", config.ClaudeFlags)
	}
}

func TestDefaultDirIsHome(t *testing.T) {
	origConfig := config
	defer func() { config = origConfig }()

	config = Config{}
	loadConfigFrom("/nonexistent/path")
	applyCLIOverrides(0, "", "", "", nil, nil)
	if config.Port != 7777 {
		t.Errorf("default port = %d, want 7777", config.Port)
	}
	if len(config.AllowedDirs) != 1 || config.AllowedDirs[0] != "~" {
		t.Errorf("default dirs = %v, want [~]", config.AllowedDirs)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /home/mutahhir/src/claude-monitor && go test -run TestCLIOverrides -v`
Expected: FAIL — `loadConfigFrom` and `applyCLIOverrides` don't exist yet.

**Step 3: Implement config + CLI**

Update `Config` struct:

```go
type Config struct {
	Port        int      `json:"port"`
	CertFile    string   `json:"cert_file"`
	KeyFile     string   `json:"key_file"`
	AuthToken   string   `json:"auth_token"`
	AllowedDirs []string `json:"allowed_dirs"`
	ClaudeFlags []string `json:"claude_flags"`
}
```

Remove the hardcoded `var allowedDirs` and replace `isAllowedDir` to use `config.AllowedDirs`:

```go
func isAllowedDir(dir string) bool {
	expanded := expandDir(dir)
	for _, d := range config.AllowedDirs {
		if expandDir(d) == expanded {
			return true
		}
	}
	return false
}
```

Add `loadConfigFrom(path)` and `applyCLIOverrides(...)`:

```go
func loadConfigFrom(path string) {
	config = Config{Port: 7777}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	json.Unmarshal(data, &config)
	if config.Port == 0 {
		config.Port = 7777
	}
}

func applyCLIOverrides(port int, certFile, keyFile, authToken string, dirs []string, claudeFlags []string) {
	if port != 0 {
		config.Port = port
	}
	if certFile != "" {
		config.CertFile = certFile
	}
	if keyFile != "" {
		config.KeyFile = keyFile
	}
	if authToken != "" {
		config.AuthToken = authToken
	}
	if len(dirs) > 0 {
		config.AllowedDirs = dirs
	}
	if len(claudeFlags) > 0 {
		config.ClaudeFlags = claudeFlags
	}
	// Default: if still no dirs, use home
	if len(config.AllowedDirs) == 0 {
		config.AllowedDirs = []string{"~"}
	}
}
```

Update `main()` to parse flags and call these:

```go
func main() {
	flagPort := flag.Int("port", 0, "HTTP port (default 7777)")
	flagCert := flag.String("cert-file", "", "TLS certificate file")
	flagKey := flag.String("key-file", "", "TLS key file")
	flagAuth := flag.String("auth-token", "", "Authentication token")
	var flagDirs stringSlice
	flag.Var(&flagDirs, "dir", "Allowed directory (repeatable)")
	var flagClaudeFlags stringSlice
	flag.Var(&flagClaudeFlags, "claude-flag", "Flag to pass to claude CLI (repeatable)")
	flag.Parse()

	loadConfigFrom(configPath())
	applyCLIOverrides(*flagPort, *flagCert, *flagKey, *flagAuth, flagDirs, flagClaudeFlags)

	// ... rest of main
}

// stringSlice implements flag.Value for repeatable --dir flags
type stringSlice []string
func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }
```

Update `handleIndex` to use `config.AllowedDirs` instead of `allowedDirs`.
Update `handleSpawn` to use `config.AllowedDirs` and `config.ClaudeFlags` instead of hardcoded values.

**Step 4: Run tests to verify they pass**

Run: `cd /home/mutahhir/src/claude-monitor && go test -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: configurable dirs and CLI flag overrides for all settings"
```

---

### Task 2: First-run Bubble Tea setup wizard

When no config.json exists and stdin is a terminal, launch an interactive wizard using `charmbracelet/huh` forms.

**Files:**
- Create: `setup.go` — wizard logic
- Modify: `main.go` — call wizard before server start
- Modify: `go.mod` — add charmbracelet dependencies

**Step 1: Add dependencies**

Run: `cd /home/mutahhir/src/claude-monitor && go get github.com/charmbracelet/huh github.com/charmbracelet/lipgloss`

**Step 2: Create `setup.go`**

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

func needsSetup() bool {
	_, err := os.Stat(configPath())
	return os.IsNotExist(err)
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func runSetupWizard() (*Config, error) {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#00e5ff")).
		MarginBottom(1)

	fmt.Println(titleStyle.Render("claude-monitor setup"))
	fmt.Println()

	cfg := &Config{Port: 7777}

	var dirsInput string
	var claudeFlagsInput string
	var portInput int
	var wantAuth bool
	var authToken string

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Allowed directories").
				Description("Comma-separated list of directories where Claude sessions can run").
				Placeholder("~/src, ~/projects").
				Value(&dirsInput),

			huh.NewMultiSelect[string]().
				Title("Claude CLI flags").
				Description("Flags passed to every claude invocation").
				Options(
					huh.NewOption("--dangerously-skip-permissions", "--dangerously-skip-permissions"),
					huh.NewOption("--chrome", "--chrome"),
					huh.NewOption("--remote-control", "--remote-control"),
					huh.NewOption("--verbose", "--verbose"),
				).
				Value(&cfg.ClaudeFlags),

			huh.NewInput().
				Title("Additional Claude flags").
				Description("Any extra flags not listed above (space-separated)").
				Placeholder("").
				Value(&claudeFlagsInput),
		),

		huh.NewGroup(
			huh.NewInput().
				Title("Port").
				Description("HTTP port to listen on").
				Placeholder("7777").
				Validate(func(s string) error {
					if s == "" {
						return nil
					}
					var p int
					if _, err := fmt.Sscanf(s, "%d", &p); err != nil || p < 1 || p > 65535 {
						return fmt.Errorf("invalid port")
					}
					return nil
				}).
				Value(func() *string {
					s := "7777"
					return &s
				}()),

			huh.NewConfirm().
				Title("Set up authentication?").
				Description("Recommended if accessible outside localhost/Tailscale").
				Value(&wantAuth),
		),
	)

	if err := form.Run(); err != nil {
		return nil, err
	}

	// Parse directories
	if dirsInput != "" {
		for _, d := range strings.Split(dirsInput, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				cfg.AllowedDirs = append(cfg.AllowedDirs, d)
			}
		}
	}
	if len(cfg.AllowedDirs) == 0 {
		cfg.AllowedDirs = []string{"~"}
	}

	// Parse extra claude flags
	if claudeFlagsInput != "" {
		for _, f := range strings.Fields(claudeFlagsInput) {
			cfg.ClaudeFlags = append(cfg.ClaudeFlags, f)
		}
	}

	// Parse port (already validated)
	fmt.Sscanf(fmt.Sprintf("%d", portInput), "%d", &cfg.Port)
	if cfg.Port == 0 {
		cfg.Port = 7777
	}

	// Auth token
	if wantAuth {
		tokenBytes := make([]byte, 16)
		rand.Read(tokenBytes)
		authToken = hex.EncodeToString(tokenBytes)
		cfg.AuthToken = authToken
		fmt.Printf("\nGenerated auth token: %s\n", authToken)
		fmt.Println("Save this — you'll need it to log in.\n")
	}

	return cfg, nil
}
```

**Step 3: Wire into main**

In `main()`, after flag parsing but before `loadConfigFrom`:

```go
if needsSetup() && isTerminal() && !flag.Parsed() {
	// No CLI flags provided and no config — run wizard
	cfg, err := runSetupWizard()
	if err != nil {
		log.Fatalf("Setup cancelled: %v", err)
	}
	config = *cfg
	persistConfig()
} else {
	loadConfigFrom(configPath())
	applyCLIOverrides(...)
}
```

Actually, simpler: run wizard only when no config exists AND no CLI flags were given AND stdin is a terminal. If any CLI flags are provided, skip the wizard and use flag+config logic.

**Step 4: Build and verify**

Run: `cd /home/mutahhir/src/claude-monitor && go build -o claude-monitor .`
Expected: Compiles.

**Step 5: Manual test**

Run with no config: `HOME=/tmp/test-home ./claude-monitor`
Expected: Interactive wizard appears.

**Step 6: Commit**

```bash
git add setup.go main.go go.mod go.sum
git commit -m "feat: add Bubble Tea first-run setup wizard"
```

---

### Task 3: Authentication middleware

Add cookie-based auth with Bearer token fallback. Only active when `auth_token` is configured.

**Files:**
- Modify: `main.go` — add auth middleware, login handler, login template
- Modify: `main_test.go` — add auth tests

**Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestAuthMiddleware(t *testing.T) {
	origConfig := config
	defer func() { config = origConfig }()

	config.AuthToken = "test-secret"

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))

	// No auth — should redirect to /login
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 302 {
		t.Errorf("no auth: got %d, want 302", rec.Code)
	}

	// Bearer token — should pass
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("bearer: got %d, want 200", rec.Code)
	}

	// Bad bearer — should redirect
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 302 {
		t.Errorf("bad bearer: got %d, want 302", rec.Code)
	}
}

func TestAuthDisabledWhenNoToken(t *testing.T) {
	origConfig := config
	defer func() { config = origConfig }()

	config.AuthToken = ""

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("no token configured: got %d, want 200", rec.Code)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /home/mutahhir/src/claude-monitor && go test -run TestAuth -v`
Expected: FAIL — `authMiddleware` doesn't exist. Also need `import "net/http/httptest"`.

**Step 3: Implement auth**

Add imports: `"crypto/hmac"`, `"crypto/sha256"`, `"crypto/subtle"`, `"encoding/hex"`, `"net/http/httptest"` (test only).

```go
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config.AuthToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Check Bearer token
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Check cookie
		if cookie, err := r.Cookie("claude-monitor-auth"); err == nil {
			if validAuthCookie(cookie.Value) {
				next.ServeHTTP(w, r)
				return
			}
		}

		http.Redirect(w, r, "/login", 302)
	})
}

func makeAuthCookie() string {
	mac := hmac.New(sha256.New, []byte(config.AuthToken))
	mac.Write([]byte("claude-monitor-auth"))
	return hex.EncodeToString(mac.Sum(nil))
}

func validAuthCookie(value string) bool {
	expected := makeAuthCookie()
	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}
```

Add login handlers:

```go
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		loginTmpl.Execute(w, nil)
		return
	}
	// POST
	r.ParseForm()
	token := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken)) != 1 {
		loginTmpl.Execute(w, map[string]bool{"Error": true})
		return
	}
	secure := config.CertFile != ""
	http.SetCookie(w, &http.Cookie{
		Name:     "claude-monitor-auth",
		Value:    makeAuthCookie(),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * 30, // 30 days
	})
	http.Redirect(w, r, "/", 302)
}
```

Add login template (minimal, matches the new clean dark style).

Wire into `main()`: register `/login` before the auth-wrapped mux, wrap all other routes with `authMiddleware`.

**Step 4: Run tests**

Run: `cd /home/mutahhir/src/claude-monitor && go test -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add optional auth with login page, cookie, and Bearer token support"
```

---

### Task 4: Add/remove directories from UI

Add API endpoints to manage allowed_dirs at runtime, persisting to config.json.

**Files:**
- Modify: `main.go` — add `handleAddDir`, `handleRemoveDir`, update config persistence
- Modify: `main_test.go` — add dir management tests

**Step 1: Write the failing test**

```go
func TestAddRemoveDir(t *testing.T) {
	origConfig := config
	defer func() { config = origConfig }()

	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)
	os.MkdirAll(filepath.Join(tmpDir, ".config", "claude-monitor"), 0755)

	config = Config{Port: 7777, AllowedDirs: []string{"~/existing"}}

	// Add a real directory
	req := httptest.NewRequest("POST", "/api/dirs", strings.NewReader(`{"dir":"`+tmpDir+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAddDir(rec, req)
	if rec.Code != 200 {
		t.Fatalf("add dir: got %d, body: %s", rec.Code, rec.Body.String())
	}
	if len(config.AllowedDirs) != 2 {
		t.Fatalf("dirs = %v, want 2 entries", config.AllowedDirs)
	}

	// Remove it
	req = httptest.NewRequest("DELETE", "/api/dirs", strings.NewReader(`{"dir":"`+tmpDir+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handleRemoveDir(rec, req)
	if rec.Code != 200 {
		t.Fatalf("remove dir: got %d", rec.Code)
	}
	if len(config.AllowedDirs) != 1 {
		t.Fatalf("dirs after remove = %v, want 1 entry", config.AllowedDirs)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /home/mutahhir/src/claude-monitor && go test -run TestAddRemoveDir -v`
Expected: FAIL — handlers don't exist.

**Step 3: Implement**

```go
func handleAddDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	var body struct{ Dir string `json:"dir"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Dir == "" {
		jsonError(w, "dir required", 400)
		return
	}
	expanded := expandDir(body.Dir)
	info, err := os.Stat(expanded)
	if err != nil || !info.IsDir() {
		jsonError(w, "directory does not exist", 400)
		return
	}
	// Check for duplicates
	for _, d := range config.AllowedDirs {
		if expandDir(d) == expanded {
			jsonError(w, "directory already added", 400)
			return
		}
	}
	config.AllowedDirs = append(config.AllowedDirs, body.Dir)
	persistConfig()
	jsonOK(w, "directory added")
}

func handleRemoveDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "DELETE only", 405)
		return
	}
	var body struct{ Dir string `json:"dir"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Dir == "" {
		jsonError(w, "dir required", 400)
		return
	}
	expanded := expandDir(body.Dir)
	filtered := config.AllowedDirs[:0]
	for _, d := range config.AllowedDirs {
		if expandDir(d) != expanded {
			filtered = append(filtered, d)
		}
	}
	config.AllowedDirs = filtered
	persistConfig()
	jsonOK(w, "directory removed")
}

func persistConfig() {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		log.Printf("persistConfig marshal: %v", err)
		return
	}
	os.MkdirAll(filepath.Dir(configPath()), 0755)
	os.WriteFile(configPath(), data, 0644)
}
```

Register in `main()`:
```go
mux.HandleFunc("/api/dirs", func(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		handleAddDir(w, r)
	case "DELETE":
		handleRemoveDir(w, r)
	default:
		http.Error(w, "method not allowed", 405)
	}
})
```

**Step 4: Run tests**

Run: `cd /home/mutahhir/src/claude-monitor && go test -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add/remove directories via API, persist to config.json"
```

---

### Task 5: UI redesign — index page

Rewrite the index page template CSS and HTML for the clean dark dashboard style. Also add the "add folder" UI.

**Files:**
- Modify: `main.go:871-1231` (the `tmpl` variable — index page template)

**Step 1: Rewrite index template**

Replace the entire index template. Key changes:
- Remove scanline overlay (`body::after`)
- Remove `border-left` accents on `.card` and `.spawn-btn`
- Increase font sizes (minimum 0.75rem, body text 0.85rem)
- Increase padding/margins (cards: 1.25rem padding, 0.75rem gaps)
- Higher contrast colors: primary text `#e2e8f0`, secondary `#94a3b8`
- Clean badges: pill-shaped with `border-radius: 9999px`
- Add folder input section: text input + Add button
- Add remove button (x) on each spawn button

This is a pure CSS/HTML change — no Go logic. Use the `frontend-design` skill for implementation.

**Step 2: Verify by building and visual check**

Run: `cd /home/mutahhir/src/claude-monitor && go build -o claude-monitor .`
Expected: Compiles without errors. Manual visual verification.

**Step 3: Commit**

```bash
git add main.go
git commit -m "redesign: clean dark dashboard UI for index page"
```

---

### Task 6: UI redesign — session page

Rewrite the session page template to match the new style.

**Files:**
- Modify: `main.go:1233-1598` (the `sessionTmpl` variable)

**Step 1: Rewrite session template**

Same style changes as index page:
- Remove scanline overlay
- Increase font sizes and whitespace
- Higher contrast text
- Clean badges
- Keep terminal container styling (xterm.js manages its own rendering)

**Step 2: Verify by building**

Run: `cd /home/mutahhir/src/claude-monitor && go build -o claude-monitor .`
Expected: Compiles.

**Step 3: Commit**

```bash
git add main.go
git commit -m "redesign: clean dark dashboard UI for session page"
```

---

### Task 7: UI redesign — login page

Style the login template to match the new dashboard style.

**Files:**
- Modify: `main.go` — the `loginTmpl` added in Task 2

**Step 1: Style login page**

Simple centered card with token input and submit button, matching the dashboard theme.

**Step 2: Build and verify**

Run: `cd /home/mutahhir/src/claude-monitor && go build -o claude-monitor .`

**Step 3: Commit**

```bash
git add main.go
git commit -m "redesign: style login page to match dashboard"
```

---

### Task 8: README

Write the README with project motivation, install instructions, config reference, and Tailscale guide.

**Files:**
- Create: `README.md`

**Step 1: Write README**

Key sections:
1. Title + one-liner: "A web dashboard for managing multiple Claude Code sessions"
2. Motivation: Claude Code's remote control is unreliable — this gives you real terminal access via browser
3. Screenshot placeholder
4. Install: `go install` or `go build`
5. Quick start: run `claude-monitor`, open browser
6. Configuration: config.json fields + CLI flags table
7. Authentication: how to set token
8. Tailscale: `tailscale serve`, TLS certs, `tailscale funnel`
9. License: MIT

**Step 2: Create LICENSE file**

Create `LICENSE` with MIT license text.

**Step 3: Commit**

```bash
git add README.md LICENSE
git commit -m "docs: add README and MIT license"
```

---

### Task 9: Remove hardcoded values and final cleanup

Remove any remaining hardcoded personal values (Tailscale hostname in log message, personal directories).

**Files:**
- Modify: `main.go:346` — remove hardcoded `valence.tail2cb751.ts.net`
- Modify: `main.go:641` — replace hardcoded flags with `config.ClaudeFlags`

**Step 1: Clean up hardcoded values**

- Replace the hardcoded Tailscale URL in log message with the actual listen address
- In `handleSpawn`, replace `flags := []string{"--dangerously-skip-permissions", "--chrome", "--remote-control"}` with `flags := append([]string{}, config.ClaudeFlags...)`

**Step 2: Run tests**

Run: `cd /home/mutahhir/src/claude-monitor && go test -v`
Expected: ALL PASS

**Step 3: Build**

Run: `cd /home/mutahhir/src/claude-monitor && go build -o claude-monitor .`
Expected: Compiles.

**Step 4: Commit**

```bash
git add main.go
git commit -m "cleanup: remove hardcoded personal values for public release"
```
