package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

var ansiRegex = regexp.MustCompile(`(\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b\[[\?]?[0-9;]*[hlmsuJKHG]|\x1b\([A-B]|\x1b[>=]|\x0f)`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// processControlCodes simulates the visual effect of in-place terminal
// animations: \r (carriage return) overwrites the line from column 0,
// \b (backspace) deletes the preceding character.
func processControlCodes(s string) string {
	if idx := strings.LastIndex(s, "\r"); idx >= 0 {
		s = s[idx+1:]
	}
	if strings.ContainsRune(s, '\b') {
		out := make([]rune, 0, len(s))
		for _, r := range s {
			if r == '\b' {
				if len(out) > 0 {
					out = out[:len(out)-1]
				}
			} else {
				out = append(out, r)
			}
		}
		s = string(out)
	}
	return s
}

var adjectives = []string{
	"swift", "bold", "calm", "dark", "eager", "fair", "glad", "hazy",
	"keen", "loud", "mild", "neat", "pale", "rare", "sage", "warm",
	"bright", "crisp", "dizzy", "fierce", "gentle", "humble", "jolly",
	"lively", "merry", "noble", "plucky", "quiet", "rustic", "vivid",
}

var nouns = []string{
	"fox", "owl", "elk", "bee", "jay", "ram", "yak", "emu",
	"lynx", "crow", "dove", "frog", "hawk", "lark", "moth", "newt",
	"orca", "puma", "rook", "swan", "vole", "wren", "wolf", "bear",
	"hare", "ibis", "kite", "mink", "quail", "robin",
}

func randomName() string {
	return adjectives[rand.Intn(len(adjectives))] + "-" + nouns[rand.Intn(len(nouns))]
}

func expandDir(dir string) string {
	if dir == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, dir[2:])
	}
	return dir
}

func isAllowedDir(dir string) bool {
	expanded := expandDir(dir)
	for _, d := range config.AllowedDirs {
		if expandDir(d) == expanded {
			return true
		}
	}
	return false
}

type Instance struct {
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	Flags     []string  `json:"flags"`
	PID       int       `json:"pid"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	ResumeID  string    `json:"resume_id,omitempty"`

	cmd         *exec.Cmd
	ptmx        *os.File
	output      *RingBuffer
	rawOutput   *RawBuffer
	broadcaster *Broadcaster
}

type SessionRecord struct {
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	Flags     []string  `json:"flags"`
	StartedAt time.Time `json:"started_at"`
}

type Config struct {
	Port        int      `json:"port"`
	CertFile    string   `json:"cert_file"`
	KeyFile     string   `json:"key_file"`
	AuthToken   string   `json:"auth_token"`
	AllowedDirs []string `json:"allowed_dirs"`
	ClaudeFlags []string `json:"claude_flags"`
}

type RingBuffer struct {
	lines   []string
	mu      sync.Mutex
	max     int
	partial string // buffered incomplete line (no trailing \n yet)
}

func NewRingBuffer(max int) *RingBuffer {
	return &RingBuffer{lines: make([]string, 0, max), max: max}
}

func (r *RingBuffer) Write(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Strip ANSI escape sequences but preserve \r and \n
	cleaned := stripANSI(s)

	// Normalize \r\n to \n so Windows-style newlines work
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")

	// Prepend any buffered partial line from a previous chunk
	cleaned = r.partial + cleaned
	r.partial = ""

	segments := strings.Split(cleaned, "\n")

	for i, seg := range segments {
		if i == len(segments)-1 {
			// Last segment has no trailing \n — buffer it as partial
			r.partial = seg
			break
		}

		// Resolve in-place overwrites (\r, \b) then store the final visual line
		line := strings.TrimSpace(processControlCodes(seg))
		if line == "" {
			continue
		}
		if len(r.lines) >= r.max {
			r.lines = r.lines[1:]
		}
		r.lines = append(r.lines, line)
	}
}

func (r *RingBuffer) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Include the current partial line (live status / progress) if non-empty
	partial := strings.TrimSpace(processControlCodes(r.partial))

	effectiveLen := len(r.lines)
	hasPartial := partial != ""
	if hasPartial {
		effectiveLen++
	}
	if n > effectiveLen {
		n = effectiveLen
	}

	storedCount := n
	if hasPartial && n > 0 {
		storedCount = n - 1
	}

	result := make([]string, 0, n)
	if storedCount > 0 {
		result = append(result, r.lines[len(r.lines)-storedCount:]...)
	}
	if hasPartial && n > 0 {
		result = append(result, partial)
	}
	return result
}

// RawBuffer is a byte-level circular buffer that stores raw PTY output
// for replay when new WebSocket clients connect.
type RawBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func NewRawBuffer(max int) *RawBuffer {
	return &RawBuffer{data: make([]byte, 0, max), max: max}
}

func (b *RawBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data)+len(p) > b.max {
		excess := len(b.data) + len(p) - b.max
		if excess >= len(b.data) {
			b.data = b.data[:0]
			if len(p) > b.max {
				p = p[len(p)-b.max:]
			}
		} else {
			b.data = b.data[excess:]
		}
	}
	b.data = append(b.data, p...)
}

func (b *RawBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}

// Broadcaster fans out raw PTY chunks to connected WebSocket clients.
type Broadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{clients: make(map[chan []byte]struct{})}
}

func (b *Broadcaster) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *Broadcaster) Send(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default:
			// client too slow, drop
		}
	}
}

var (
	instances = make(map[string]*Instance)
	mu        sync.RWMutex
	config    Config
)

var sessionOrder []string // tracks instance names in spawn order

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

	// Determine whether any CLI flags were explicitly provided.
	cliProvided := *flagPort != 0 || *flagCert != "" || *flagKey != "" ||
		*flagAuth != "" || len(flagDirs) > 0 || len(flagClaudeFlags) > 0

	if needsSetup() && isTerminal() && !cliProvided {
		// First run: launch interactive setup wizard.
		cfg, err := runSetupWizard()
		if err != nil {
			log.Fatalf("Setup wizard failed: %v", err)
		}
		config = *cfg
		persistConfig()
		fmt.Printf("\nConfig saved to %s\n", configPath())
	} else {
		// Normal path: load existing config, apply CLI overrides.
		loadConfigFrom(configPath())
		applyCLIOverrides(*flagPort, *flagCert, *flagKey, *flagAuth, flagDirs, flagClaudeFlags)
	}

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

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/session/", handleSession)
	mux.HandleFunc("/spawn", handleSpawn)
	mux.HandleFunc("/kill/", handleKill)
	mux.HandleFunc("/resume/", handleResume)
	mux.HandleFunc("/restart/", handleRestart)
	mux.HandleFunc("/api/instances", handleAPIInstances)
	mux.HandleFunc("/api/output/", handleAPIOutput)
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
	mux.HandleFunc("/ws/", handleWS)

	handler := authMiddleware(mux)

	addr := fmt.Sprintf(":%d", config.Port)

	if config.CertFile != "" && config.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			log.Fatalf("Failed to load TLS cert/key: %v", err)
		}
		srv := &http.Server{
			Addr:    addr,
			Handler: handler,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
			},
		}
		log.Printf("claude-monitor listening on https://0.0.0.0%s", addr)
		log.Fatal(srv.ListenAndServeTLS("", ""))
	} else {
		log.Printf("claude-monitor listening on http://localhost%s (no TLS certs configured)", addr)
		log.Fatal(http.ListenAndServe(addr, handler))
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-monitor", "config.json")
}

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

// buildSessionRecords collects session metadata. Caller must hold mu (read or write).
func buildSessionRecords() []SessionRecord {
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
	return records
}

// writeSessionsToDisk persists session records to disk. Must NOT be called under mu.
func writeSessionsToDisk(records []SessionRecord) {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		log.Printf("saveSessions marshal: %v", err)
		return
	}
	os.MkdirAll(filepath.Dir(sessionsFilePath()), 0755)
	tmp := sessionsFilePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("saveSessions write: %v", err)
		return
	}
	if err := os.Rename(tmp, sessionsFilePath()); err != nil {
		log.Printf("saveSessions rename: %v", err)
	}
}

// saveRawOutput writes an instance's raw PTY buffer to disk.
func saveRawOutput(name string, raw *RawBuffer) {
	os.MkdirAll(sessionsDir(), 0755)
	data := raw.Bytes()
	if err := os.WriteFile(sessionRawPath(name), data, 0644); err != nil {
		log.Printf("saveRawOutput %s: %v", name, err)
	}
}

// loadSessions restores saved sessions as stopped instances on startup.
// Must be called before the HTTP server starts (single-threaded startup).
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
	if len(config.AllowedDirs) == 0 {
		config.AllowedDirs = []string{"~"}
	}
}

// persistConfig writes the current config to configPath() as indented JSON.
func persistConfig() {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		log.Printf("persistConfig marshal: %v", err)
		return
	}
	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("persistConfig mkdir: %v", err)
		return
	}
	if err := os.WriteFile(configPath(), data, 0o644); err != nil {
		log.Printf("persistConfig write: %v", err)
	}
}

// stringSlice implements flag.Value for repeatable flags.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func spawnInstance(name, dir string, flags []string, resume bool) error {
	mu.Lock()

	if inst, ok := instances[name]; ok && inst.Status == "running" {
		mu.Unlock()
		return fmt.Errorf("instance %q already running", name)
	}

	args := append([]string{}, flags...)
	if resume {
		args = append(args, "-r", name)
	} else {
		args = append(args, "-n", name)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		mu.Unlock()
		return fmt.Errorf("pty start: %w", err)
	}
	// Set a mobile-friendly default size; clients will resize on connect
	pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})

	buf := NewRingBuffer(500)
	rawBuf := NewRawBuffer(256 * 1024) // 256KB replay buffer
	bc := NewBroadcaster()
	inst := &Instance{
		Name:        name,
		Dir:         dir,
		Flags:       flags,
		PID:         cmd.Process.Pid,
		Status:      "running",
		StartedAt:   time.Now(),
		cmd:         cmd,
		ptmx:        ptmx,
		output:      buf,
		rawOutput:   rawBuf,
		broadcaster: bc,
	}
	instances[name] = inst

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
	records := buildSessionRecords()
	mu.Unlock()

	// Persist outside the lock to avoid blocking other handlers during disk I/O
	writeSessionsToDisk(records)

	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := ptmx.Read(tmp)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, tmp[:n])
				buf.Write(string(chunk))
				rawBuf.Write(chunk)
				bc.Send(chunk)
			}
			if err != nil {
				break
			}
		}
	}()

	go func() {
		cmd.Wait()
		mu.Lock()
		inst.Status = "stopped"
		inst.PID = 0
		inst.ResumeID = name
		mu.Unlock()
		saveRawOutput(name, inst.rawOutput)
	}()

	return nil
}

func killInstance(name string) error {
	mu.Lock()
	inst, ok := instances[name]
	mu.Unlock()

	if !ok {
		return fmt.Errorf("instance %q not found", name)
	}
	if inst.Status != "running" {
		return fmt.Errorf("instance %q not running", name)
	}

	inst.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		inst.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		inst.cmd.Process.Kill()
		<-done
	}
	return nil
}

// --- Auth Middleware & Helpers ---

// authMiddleware enforces authentication when config.AuthToken is set.
// It passes through all requests if no token is configured.
// The /login path is always accessible without auth.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No auth configured — pass through
		if config.AuthToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Always allow access to /login
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization: Bearer <token>
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Check auth cookie
		if cookie, err := r.Cookie("claude-monitor-auth"); err == nil {
			if validAuthCookie(cookie.Value) {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Not authenticated — redirect to login
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// makeAuthCookie returns an HMAC-SHA256 of "claude-monitor-auth" keyed with
// config.AuthToken, hex-encoded. This is the value stored in the auth cookie.
func makeAuthCookie() string {
	mac := hmac.New(sha256.New, []byte(config.AuthToken))
	mac.Write([]byte("claude-monitor-auth"))
	return hex.EncodeToString(mac.Sum(nil))
}

// validAuthCookie performs a constant-time comparison of a cookie value
// against the expected HMAC.
func validAuthCookie(value string) bool {
	expected := makeAuthCookie()
	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

// handleLogin serves the login page (GET) and processes login attempts (POST).
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if config.AuthToken == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if r.Method == "GET" {
		loginTmpl.Execute(w, map[string]string{})
		return
	}

	if r.Method == "POST" {
		r.ParseForm()
		token := r.FormValue("token")

		if subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken)) != 1 {
			loginTmpl.Execute(w, map[string]string{"Error": "Invalid token"})
			return
		}

		// Set auth cookie
		http.SetCookie(w, &http.Cookie{
			Name:     "claude-monitor-auth",
			Value:    makeAuthCookie(),
			Path:     "/",
			HttpOnly: true,
			Secure:   config.CertFile != "",
			SameSite: http.SameSiteStrictMode,
			MaxAge:   30 * 24 * 60 * 60, // 30 days
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

var loginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="theme-color" content="#0d1117">
<title>Login — claude-monitor</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;700&display=swap" rel="stylesheet">
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: 'JetBrains Mono', 'SF Mono', 'Consolas', monospace;
    background: #0d1117; color: #e2e8f0;
    display: flex; align-items: center; justify-content: center;
    min-height: 100vh;
    padding: 1rem;
  }
  .login-box {
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid rgba(255, 255, 255, 0.1);
    border-radius: 8px;
    padding: 2.5rem 2rem;
    width: 100%; max-width: 380px;
  }
  h1 {
    font-size: 1rem;
    font-weight: 700;
    color: #e2e8f0;
    margin-bottom: 2rem;
    text-align: center;
  }
  label {
    display: block;
    font-size: 0.75rem;
    color: #8b949e;
    margin-bottom: 0.5rem;
    font-weight: 500;
  }
  input[type="password"] {
    width: 100%;
    padding: 0.7rem 0.85rem;
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid rgba(255, 255, 255, 0.1);
    border-radius: 6px;
    color: #e2e8f0;
    font-family: inherit;
    font-size: 0.85rem;
    outline: none;
    margin-bottom: 1.5rem;
    transition: border-color 0.15s ease;
  }
  input[type="password"]::placeholder { color: #8b949e; }
  input[type="password"]:focus {
    border-color: #58a6ff;
  }
  button {
    width: 100%;
    padding: 0.7rem;
    background: rgba(88, 166, 255, 0.15);
    border: 1px solid rgba(88, 166, 255, 0.3);
    border-radius: 6px;
    color: #58a6ff;
    font-family: inherit;
    font-size: 0.85rem;
    font-weight: 600;
    cursor: pointer;
    transition: all 0.15s ease;
  }
  button:hover { background: rgba(88, 166, 255, 0.25); }
  .error {
    color: #f85149;
    font-size: 0.85rem;
    margin-bottom: 1.25rem;
    text-align: center;
    padding: 0.6rem;
    background: rgba(248, 81, 73, 0.1);
    border: 1px solid rgba(248, 81, 73, 0.2);
    border-radius: 6px;
  }
</style>
</head>
<body>
<div class="login-box">
  <h1>claude-monitor</h1>
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
  <form method="POST" action="/login">
    <label for="token-input">Auth token</label>
    <input type="password" id="token-input" name="token" placeholder="Enter your token" autofocus>
    <button type="submit">Login</button>
  </form>
</div>
</body>
</html>`))

// --- HTTP Handlers ---

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
	}{ordered, config.AllowedDirs}

	tmpl.Execute(w, data)
}

func handleSpawn(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	r.ParseForm()
	dir := r.FormValue("dir")

	if dir == "" {
		jsonError(w, "dir required", 400)
		return
	}
	if !isAllowedDir(dir) {
		jsonError(w, "directory not allowed", 400)
		return
	}

	name := randomName()
	// Ensure unique name
	mu.RLock()
	for {
		if _, exists := instances[name]; !exists {
			break
		}
		name = randomName()
	}
	mu.RUnlock()

	flags := config.ClaudeFlags

	if err := spawnInstance(name, expandDir(dir), flags, false); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	jsonOK(w, name+" spawned")
}

func handleKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/kill/")
	if err := killInstance(name); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	time.Sleep(500 * time.Millisecond)
	jsonOK(w, "killed")
}

func handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/resume/")

	mu.RLock()
	inst, ok := instances[name]
	mu.RUnlock()

	if !ok {
		jsonError(w, "not found", 404)
		return
	}
	if inst.Status == "running" {
		jsonError(w, "already running", 400)
		return
	}

	if err := spawnInstance(name, inst.Dir, inst.Flags, true); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, "resumed")
}

func handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/restart/")

	mu.RLock()
	inst, ok := instances[name]
	mu.RUnlock()

	if !ok {
		jsonError(w, "not found", 404)
		return
	}

	dir := inst.Dir
	flags := inst.Flags

	if inst.Status == "running" {
		if err := killInstance(name); err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		time.Sleep(1 * time.Second)
	}

	if err := spawnInstance(name, dir, flags, true); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, "restarted")
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/session/")
	mu.RLock()
	inst, ok := instances[name]
	mu.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	data := struct {
		Instance *Instance
		Output   []string
	}{inst, inst.output.Last(200)}

	sessionTmpl.Execute(w, data)
}

func handleAPIOutput(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/output/")
	mu.RLock()
	inst, ok := instances[name]
	mu.RUnlock()

	if !ok {
		jsonError(w, "not found", 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"name":   inst.Name,
		"status": inst.Status,
		"output": inst.output.Last(200),
	})
}

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

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ws/")
	mu.RLock()
	inst, ok := instances[name]
	mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Send buffered raw output so the client can reconstruct current screen
	replay := inst.rawOutput.Bytes()
	if len(replay) > 0 {
		if err := conn.WriteMessage(websocket.BinaryMessage, replay); err != nil {
			return
		}
	}

	// Subscribe to live output
	ch := inst.broadcaster.Subscribe()
	defer inst.broadcaster.Unsubscribe(ch)

	// Read pump: handle resize, input, and detect connection close
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			msgType, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.TextMessage {
				// JSON control messages: resize or input
				var ctrl struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
					Data string `json:"data"`
				}
				if json.Unmarshal(msg, &ctrl) != nil {
					continue
				}
				switch ctrl.Type {
				case "resize":
					if ctrl.Cols > 0 && ctrl.Rows > 0 && inst.ptmx != nil {
						pty.Setsize(inst.ptmx, &pty.Winsize{Rows: ctrl.Rows, Cols: ctrl.Cols})
					}
				case "input":
					if inst.ptmx != nil && inst.Status == "running" {
						inst.ptmx.WriteString(ctrl.Data)
					}
				}
			}
		}
	}()

	// Write pump: forward live PTY chunks to the WebSocket
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return
			}
		case <-closed:
			return
		}
	}
}

func jsonOK(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": msg})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": msg})
}

func handleAddDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	var body struct {
		Dir string `json:"dir"`
	}
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
	var body struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Dir == "" {
		jsonError(w, "dir required", 400)
		return
	}
	expanded := expandDir(body.Dir)
	filtered := make([]string, 0, len(config.AllowedDirs))
	for _, d := range config.AllowedDirs {
		if expandDir(d) != expanded {
			filtered = append(filtered, d)
		}
	}
	config.AllowedDirs = filtered
	persistConfig()
	jsonOK(w, "directory removed")
}

var tmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, user-scalable=no">
<meta name="theme-color" content="#0d1117">
<title>claude-monitor</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;700&display=swap" rel="stylesheet">
<style>
  :root {
    --bg: #0d1117;
    --surface: rgba(255, 255, 255, 0.04);
    --border: rgba(255, 255, 255, 0.1);
    --border-hi: rgba(255, 255, 255, 0.2);
    --accent: #58a6ff;
    --accent-dim: rgba(88, 166, 255, 0.15);
    --green: #3fb950;
    --green-dim: rgba(63, 185, 80, 0.15);
    --red: #f85149;
    --red-dim: rgba(248, 81, 73, 0.15);
    --amber: #d29922;
    --amber-dim: rgba(210, 153, 34, 0.15);
    --text: #e2e8f0;
    --text-secondary: #8b949e;
    --mono: 'JetBrains Mono', 'SF Mono', 'Consolas', monospace;
  }

  * { box-sizing: border-box; margin: 0; padding: 0; }

  body {
    font-family: var(--mono);
    background: var(--bg); color: var(--text);
    padding: 2rem; padding-bottom: 6rem;
    -webkit-text-size-adjust: 100%;
    min-height: 100vh;
  }

  /* Title */
  .title {
    font-size: 1rem;
    font-weight: 700;
    color: var(--text);
    margin-bottom: 2rem;
    padding-bottom: 1rem;
    border-bottom: 1px solid var(--border);
  }

  /* Toast */
  .toast {
    position: fixed; top: 0; left: 0; right: 0;
    padding: 0.85rem 1.5rem;
    font-size: 0.85rem; font-family: var(--mono);
    font-weight: 500;
    z-index: 100;
    transform: translateY(-100%);
    transition: transform 0.3s cubic-bezier(0.16, 1, 0.3, 1);
    pointer-events: none;
    text-align: center;
    letter-spacing: 0.02em;
  }
  .toast.ok {
    background: var(--green-dim);
    color: var(--green);
    border-bottom: 1px solid rgba(63, 185, 80, 0.3);
  }
  .toast.err {
    background: var(--red-dim);
    color: var(--red);
    border-bottom: 1px solid rgba(248, 81, 73, 0.3);
  }
  .toast.show { transform: translateY(0); }

  /* Section labels */
  .section-label {
    font-size: 0.75rem; color: var(--text-secondary);
    text-transform: uppercase; letter-spacing: 0.1em;
    margin-bottom: 0.75rem;
    font-weight: 500;
  }

  /* Add folder section */
  .add-folder {
    display: flex; gap: 0.5rem;
    margin-bottom: 1.25rem;
  }
  .add-folder input {
    flex: 1;
    padding: 0.6rem 0.85rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 0.85rem;
    outline: none;
    transition: border-color 0.15s ease;
  }
  .add-folder input::placeholder { color: var(--text-secondary); }
  .add-folder input:focus { border-color: var(--accent); }
  .add-folder button {
    padding: 0.6rem 1rem;
    background: var(--accent-dim);
    border: 1px solid rgba(88, 166, 255, 0.3);
    border-radius: 6px;
    color: var(--accent);
    font-family: var(--mono);
    font-size: 0.85rem;
    font-weight: 600;
    cursor: pointer;
    white-space: nowrap;
    transition: all 0.15s ease;
  }
  .add-folder button:hover { background: rgba(88, 166, 255, 0.25); }

  /* Spawn grid */
  .spawn-grid {
    display: grid; grid-template-columns: 1fr 1fr; gap: 0.75rem;
    margin-bottom: 2.5rem;
  }
  .spawn-btn-wrap {
    position: relative;
  }
  .spawn-btn {
    width: 100%;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.85rem 2.5rem 0.85rem 1rem;
    color: var(--text);
    font-family: var(--mono);
    font-size: 0.85rem;
    font-weight: 500;
    cursor: pointer;
    -webkit-tap-highlight-color: transparent;
    transition: all 0.15s ease;
    text-align: left;
    position: relative;
    overflow: hidden;
  }
  .spawn-btn::after {
    content: '+';
    position: absolute;
    right: 0.85rem; top: 50%;
    transform: translateY(-50%);
    color: var(--text-secondary);
    font-size: 1.1rem;
    font-weight: 400;
  }
  .spawn-btn:hover {
    background: rgba(255, 255, 255, 0.06);
    border-color: var(--border-hi);
  }
  .spawn-btn:active {
    transform: scale(0.98);
  }
  .spawn-btn .dir-name {
    color: var(--accent);
    font-weight: 600;
  }
  .remove-dir-btn {
    position: absolute;
    top: -6px; right: -6px;
    width: 20px; height: 20px;
    border-radius: 50%;
    background: var(--red-dim);
    border: 1px solid rgba(248, 81, 73, 0.3);
    color: var(--red);
    font-size: 0.7rem;
    font-family: var(--mono);
    cursor: pointer;
    display: flex; align-items: center; justify-content: center;
    line-height: 1;
    opacity: 0;
    transition: opacity 0.15s ease;
    z-index: 2;
  }
  .spawn-btn-wrap:hover .remove-dir-btn { opacity: 1; }
  .remove-dir-btn:hover { background: rgba(248, 81, 73, 0.3); }

  /* Session cards */
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 1.25rem;
    margin-bottom: 0.75rem;
    cursor: pointer;
    transition: all 0.15s ease;
    position: relative;
    animation: cardIn 0.4s cubic-bezier(0.16, 1, 0.3, 1) both;
  }
  .card:hover {
    background: rgba(255, 255, 255, 0.06);
    border-color: var(--border-hi);
  }
  .card:active {
    transform: scale(0.99);
  }

  @keyframes cardIn {
    from { opacity: 0; transform: translateY(8px); }
    to { opacity: 1; transform: translateY(0); }
  }

  .card-header {
    display: flex; justify-content: space-between; align-items: center;
    margin-bottom: 0.5rem;
  }
  .card-name {
    font-weight: 700; font-size: 1rem; color: var(--text);
    letter-spacing: 0.01em;
  }

  .badge {
    font-size: 0.75rem; font-family: var(--mono);
    padding: 0.2rem 0.65rem; border-radius: 9999px;
    font-weight: 600; letter-spacing: 0.02em;
    display: flex; align-items: center; gap: 0.35rem;
  }
  .badge-running {
    background: var(--green-dim); color: var(--green);
  }
  .badge-running::before {
    content: '';
    width: 6px; height: 6px;
    background: var(--green);
    border-radius: 50%;
    animation: pulse-dot 2s ease-in-out infinite;
  }
  @keyframes pulse-dot {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.4; }
  }
  .badge-stopped {
    background: var(--red-dim); color: var(--red);
  }

  .card-meta {
    font-size: 0.8rem; color: var(--text-secondary);
    margin-bottom: 0.85rem;
    font-weight: 400;
  }
  .card-meta span { margin-right: 1rem; }

  .card-actions { display: flex; gap: 0.5rem; }

  .card-actions button {
    flex: 1; padding: 0.5rem 0;
    border-radius: 5px;
    font-size: 0.8rem; font-weight: 600; font-family: var(--mono);
    letter-spacing: 0.02em;
    cursor: pointer;
    -webkit-tap-highlight-color: transparent;
    transition: all 0.15s ease;
  }
  .card-actions button:active { transform: scale(0.96); }
  .card-actions button:disabled { opacity: 0.3; cursor: not-allowed; transform: none; }

  .btn-kill {
    background: var(--red-dim); color: var(--red);
    border: 1px solid rgba(248, 81, 73, 0.3);
  }
  .btn-kill:hover { background: rgba(248, 81, 73, 0.25); }
  .btn-resume {
    background: var(--green-dim); color: var(--green);
    border: 1px solid rgba(63, 185, 80, 0.3);
  }
  .btn-resume:hover { background: rgba(63, 185, 80, 0.25); }
  .btn-restart {
    background: var(--amber-dim); color: var(--amber);
    border: 1px solid rgba(210, 153, 34, 0.3);
  }
  .btn-restart:hover { background: rgba(210, 153, 34, 0.25); }

  .empty {
    color: var(--text-secondary); text-align: center;
    padding: 3rem 0; font-size: 0.85rem;
    letter-spacing: 0.02em;
    border: 1px dashed var(--border);
    border-radius: 6px;
  }

  /* Stagger card animations */
  .card:nth-child(1) { animation-delay: 0s; }
  .card:nth-child(2) { animation-delay: 0.05s; }
  .card:nth-child(3) { animation-delay: 0.1s; }
  .card:nth-child(4) { animation-delay: 0.15s; }
  .card:nth-child(5) { animation-delay: 0.2s; }
  .card:nth-child(6) { animation-delay: 0.25s; }
  .card:nth-child(7) { animation-delay: 0.3s; }
  .card:nth-child(8) { animation-delay: 0.35s; }
</style>
</head>
<body>

<div class="toast" id="toast"></div>

<div class="title">claude-monitor</div>

<div class="section-label">Add folder</div>
<div class="add-folder">
  <input type="text" id="new-dir-input" placeholder="/path/to/project">
  <button onclick="addDir()">Add</button>
</div>

<div class="section-label">Launch session</div>
<div class="spawn-grid">
  {{range .AllowedDirs}}
  <div class="spawn-btn-wrap">
    <button class="spawn-btn" onclick="spawnIn('{{.}}')">
      <span class="dir-name">{{.}}</span>
    </button>
    <button class="remove-dir-btn" onclick="removeDir('{{.}}')" title="Remove directory">x</button>
  </div>
  {{end}}
</div>

<div class="section-label">Sessions</div>
<div id="instances">
  {{range .Instances}}
  <div class="card" onclick="location.href='/session/{{.Name}}'">
    <div class="card-header">
      <span class="card-name">{{.Name}}</span>
      <span class="badge badge-{{.Status}}">{{.Status}}</span>
    </div>
    <div class="card-meta">
      <span>{{.Dir}}</span>
      {{if eq .Status "running"}}<span>pid {{.PID}}</span>{{end}}
    </div>
    <div class="card-actions">
      {{if eq .Status "running"}}
        <button class="btn-kill" onclick="event.stopPropagation();action('kill','{{.Name}}',this)">Kill</button>
        <button class="btn-restart" onclick="event.stopPropagation();action('restart','{{.Name}}',this)">Restart</button>
      {{else}}
        <button class="btn-resume" onclick="event.stopPropagation();action('resume','{{.Name}}',this)">Resume</button>
      {{end}}
    </div>
  </div>
  {{else}}
  <div class="empty">No active sessions</div>
  {{end}}
</div>

<script>
function toast(msg, ok) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast ' + (ok ? 'ok' : 'err') + ' show';
  setTimeout(() => t.classList.remove('show'), 2000);
}

async function action(act, name, btn) {
  if (btn) btn.disabled = true;
  try {
    const res = await fetch('/' + act + '/' + encodeURIComponent(name), { method: 'POST' });
    const data = await res.json();
    if (data.status === 'ok') {
      toast(name + ': ' + data.message, true);
      setTimeout(() => location.reload(), 800);
    } else {
      toast(data.message, false);
      if (btn) btn.disabled = false;
    }
  } catch (e) {
    toast('Request failed', false);
    if (btn) btn.disabled = false;
  }
}

async function spawnIn(dir) {
  const form = new URLSearchParams({ dir });
  try {
    const res = await fetch('/spawn', { method: 'POST', body: form });
    const data = await res.json();
    if (data.status === 'ok') {
      toast(data.message, true);
      setTimeout(() => location.reload(), 800);
    } else {
      toast(data.message, false);
    }
  } catch (e) {
    toast('Request failed', false);
  }
}

async function addDir() {
  const input = document.getElementById('new-dir-input');
  const dir = input.value.trim();
  if (!dir) return;
  try {
    const res = await fetch('/api/dirs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ dir: dir })
    });
    const data = await res.json();
    if (data.status === 'ok') {
      toast(data.message, true);
      setTimeout(() => location.reload(), 800);
    } else {
      toast(data.message, false);
    }
  } catch (e) {
    toast('Request failed', false);
  }
}

async function removeDir(dir) {
  if (!confirm('Remove ' + dir + ' from allowed directories?')) return;
  try {
    const res = await fetch('/api/dirs', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ dir: dir })
    });
    const data = await res.json();
    if (data.status === 'ok') {
      toast(data.message, true);
      setTimeout(() => location.reload(), 800);
    } else {
      toast(data.message, false);
    }
  } catch (e) {
    toast('Request failed', false);
  }
}

setInterval(() => {
  fetch('/api/instances').then(r => r.json()).then(() => {
    location.reload();
  }).catch(() => {});
}, 10000);
</script>

</body>
</html>
`))

var sessionTmpl = template.Must(template.New("session").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, user-scalable=no">
<meta name="theme-color" content="#0d1117">
<title>{{.Instance.Name}} — claude-monitor</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;700&display=swap" rel="stylesheet">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.min.css">
<style>
  :root {
    --bg: #0d1117;
    --surface: rgba(255, 255, 255, 0.04);
    --border: rgba(255, 255, 255, 0.1);
    --border-hi: rgba(255, 255, 255, 0.2);
    --accent: #58a6ff;
    --green: #3fb950;
    --green-dim: rgba(63, 185, 80, 0.15);
    --red: #f85149;
    --red-dim: rgba(248, 81, 73, 0.15);
    --amber: #d29922;
    --amber-dim: rgba(210, 153, 34, 0.15);
    --text: #e2e8f0;
    --text-secondary: #8b949e;
    --mono: 'JetBrains Mono', 'SF Mono', 'Consolas', monospace;
  }

  * { box-sizing: border-box; margin: 0; padding: 0; }

  body {
    font-family: var(--mono);
    background: var(--bg); color: var(--text);
    padding: 1rem; padding-bottom: 0;
    -webkit-text-size-adjust: 100%;
    display: flex; flex-direction: column; height: 100vh;
    height: 100dvh;
    overflow: hidden;
  }

  /* Toast */
  .toast {
    position: fixed; top: 0; left: 0; right: 0;
    padding: 0.85rem 1.5rem;
    font-size: 0.85rem; font-family: var(--mono);
    font-weight: 500;
    z-index: 100;
    transform: translateY(-100%);
    transition: transform 0.3s cubic-bezier(0.16, 1, 0.3, 1);
    pointer-events: none;
    text-align: center;
    letter-spacing: 0.02em;
  }
  .toast.ok {
    background: var(--green-dim); color: var(--green);
    border-bottom: 1px solid rgba(63, 185, 80, 0.3);
  }
  .toast.err {
    background: var(--red-dim); color: var(--red);
    border-bottom: 1px solid rgba(248, 81, 73, 0.3);
  }
  .toast.show { transform: translateY(0); }

  /* Top bar */
  .topbar {
    display: flex; align-items: center; justify-content: space-between;
    margin-bottom: 0.75rem;
    flex-shrink: 0;
  }
  .back {
    color: var(--accent); text-decoration: none;
    font-size: 0.85rem; font-weight: 500;
    letter-spacing: 0.02em;
    padding: 0.3rem 0;
    display: flex; align-items: center; gap: 0.4rem;
    transition: color 0.15s ease;
  }
  .back:hover { color: var(--text); }
  .back::before {
    content: '\2190';
    font-size: 1rem;
  }

  .ws-status {
    font-size: 0.75rem; color: var(--text-secondary);
    letter-spacing: 0.03em;
    display: flex; align-items: center; gap: 0.4rem;
  }
  .ws-status::before {
    content: '';
    width: 6px; height: 6px;
    border-radius: 50%;
    background: var(--text-secondary);
    flex-shrink: 0;
  }
  .ws-status.connected { color: var(--green); }
  .ws-status.connected::before {
    background: var(--green);
    animation: pulse-dot 2s ease-in-out infinite;
  }
  .ws-status.error { color: var(--red); }
  .ws-status.error::before { background: var(--red); }

  @keyframes pulse-dot {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.4; }
  }

  /* Session header */
  .session-header {
    display: flex; align-items: center; justify-content: space-between;
    padding-bottom: 0.75rem;
    margin-bottom: 0.75rem;
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }
  .session-name {
    font-size: 1rem; font-weight: 700; color: var(--text);
    letter-spacing: 0.01em;
  }
  .badge {
    font-size: 0.75rem; font-family: var(--mono);
    padding: 0.2rem 0.65rem; border-radius: 9999px;
    font-weight: 600; letter-spacing: 0.02em;
    display: flex; align-items: center; gap: 0.35rem;
  }
  .badge-running {
    background: var(--green-dim); color: var(--green);
  }
  .badge-running::before {
    content: '';
    width: 6px; height: 6px;
    background: var(--green);
    border-radius: 50%;
    animation: pulse-dot 2s ease-in-out infinite;
  }
  .badge-stopped {
    background: var(--red-dim); color: var(--red);
  }

  .meta {
    font-size: 0.8rem; color: var(--text-secondary);
    margin-bottom: 0.75rem;
    flex-shrink: 0;
  }
  .meta span { margin-right: 1rem; }

  /* Actions */
  .actions { display: flex; gap: 0.5rem; margin-bottom: 0.75rem; flex-shrink: 0; }
  .actions button {
    flex: 1; padding: 0.5rem 0;
    border-radius: 5px;
    font-size: 0.8rem; font-weight: 600; font-family: var(--mono);
    letter-spacing: 0.02em;
    cursor: pointer;
    -webkit-tap-highlight-color: transparent;
    transition: all 0.15s ease;
  }
  .actions button:active { transform: scale(0.96); }
  .actions button:disabled { opacity: 0.3; cursor: not-allowed; transform: none; }
  .btn-kill {
    background: var(--red-dim); color: var(--red);
    border: 1px solid rgba(248, 81, 73, 0.3);
  }
  .btn-kill:hover { background: rgba(248, 81, 73, 0.25); }
  .btn-resume {
    background: var(--green-dim); color: var(--green);
    border: 1px solid rgba(63, 185, 80, 0.3);
  }
  .btn-resume:hover { background: rgba(63, 185, 80, 0.25); }
  .btn-restart {
    background: var(--amber-dim); color: var(--amber);
    border: 1px solid rgba(210, 153, 34, 0.3);
  }
  .btn-restart:hover { background: rgba(210, 153, 34, 0.25); }

  /* Terminal */
  #terminal-container {
    flex: 1; min-height: 0;
    background: #0d1117;
    border: 1px solid var(--border);
    border-bottom: none;
    border-radius: 6px 6px 0 0;
    padding: 4px;
    overflow: hidden;
  }
</style>
</head>
<body>

<div class="toast" id="toast"></div>

<div class="topbar">
  <a class="back" href="/">back</a>
  <div class="ws-status" id="ws-status">connecting</div>
</div>

<div class="session-header">
  <span class="session-name">{{.Instance.Name}}</span>
  <span class="badge badge-{{.Instance.Status}}">{{.Instance.Status}}</span>
</div>

<div class="meta">
  <span>{{.Instance.Dir}}</span>
  {{if eq .Instance.Status "running"}}<span>pid {{.Instance.PID}}</span>{{end}}
</div>

<div class="actions">
  {{if eq .Instance.Status "running"}}
    <button class="btn-kill" onclick="action('kill','{{.Instance.Name}}',this)">Kill</button>
    <button class="btn-restart" onclick="action('restart','{{.Instance.Name}}',this)">Restart</button>
  {{else}}
    <button class="btn-resume" onclick="action('resume','{{.Instance.Name}}',this)">Resume</button>
  {{end}}
</div>

<div id="terminal-container"></div>

<script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.11.0/lib/addon-fit.js"></script>
<script>
const name = '{{.Instance.Name}}';

function toast(msg, ok) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast ' + (ok ? 'ok' : 'err') + ' show';
  setTimeout(() => t.classList.remove('show'), 2000);
}

async function action(act, n, btn) {
  if (btn) btn.disabled = true;
  try {
    const res = await fetch('/' + act + '/' + encodeURIComponent(n), { method: 'POST' });
    const data = await res.json();
    if (data.status === 'ok') {
      toast(n + ': ' + data.message, true);
      setTimeout(() => location.reload(), 800);
    } else {
      toast(data.message, false);
      if (btn) btn.disabled = false;
    }
  } catch (e) {
    toast('Request failed', false);
    if (btn) btn.disabled = false;
  }
}

// --- xterm.js terminal ---
const term = new Terminal({
  cursorBlink: true,
  cursorStyle: 'bar',
  fontSize: 13,
  fontFamily: "'JetBrains Mono', 'SF Mono', 'Menlo', 'Consolas', monospace",
  fontWeight: '400',
  fontWeightBold: '700',
  theme: {
    background: '#0d1117',
    foreground: '#e2e8f0',
    cursor: '#58a6ff',
    cursorAccent: '#0d1117',
    selectionBackground: 'rgba(88, 166, 255, 0.2)',
    selectionForeground: '#ffffff',
    black: '#1a1a2e',
    red: '#f85149',
    green: '#3fb950',
    yellow: '#d29922',
    blue: '#58a6ff',
    magenta: '#bc8cff',
    cyan: '#39d2e0',
    white: '#e2e8f0',
    brightBlack: '#8b949e',
    brightRed: '#ff7b72',
    brightGreen: '#56d364',
    brightYellow: '#e3b341',
    brightBlue: '#79c0ff',
    brightMagenta: '#d2a8ff',
    brightCyan: '#56d4dd',
    brightWhite: '#f0f6fc',
  },
  scrollback: 5000,
  convertEol: false,
});

const fitAddon = new FitAddon.FitAddon();
term.loadAddon(fitAddon);
term.open(document.getElementById('terminal-container'));
fitAddon.fit();

// --- WebSocket connection ---
const statusEl = document.getElementById('ws-status');
let currentWS = null;

function sendResize() {
  if (currentWS && currentWS.readyState === WebSocket.OPEN) {
    currentWS.send(JSON.stringify({
      type: 'resize', cols: term.cols, rows: term.rows
    }));
  }
}

function fitAndResize() {
  fitAddon.fit();
  sendResize();
}

window.addEventListener('resize', fitAndResize);
new ResizeObserver(fitAndResize).observe(document.getElementById('terminal-container'));

term.onData((data) => {
  if (currentWS && currentWS.readyState === WebSocket.OPEN) {
    currentWS.send(JSON.stringify({ type: 'input', data }));
  }
});

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(proto + '//' + location.host + '/ws/' + encodeURIComponent(name));
  ws.binaryType = 'arraybuffer';
  currentWS = ws;

  ws.onopen = () => {
    statusEl.textContent = 'connected';
    statusEl.className = 'ws-status connected';
    sendResize();
  };

  ws.onmessage = (evt) => {
    const data = new Uint8Array(evt.data);
    term.write(data, () => {
      term.scrollToBottom();
    });
  };

  ws.onclose = () => {
    statusEl.textContent = 'reconnecting';
    statusEl.className = 'ws-status error';
    currentWS = null;
    setTimeout(connectWS, 2000);
  };

  ws.onerror = () => {
    statusEl.textContent = 'error';
    statusEl.className = 'ws-status error';
  };
}

connectWS();
</script>

</body>
</html>
`))
