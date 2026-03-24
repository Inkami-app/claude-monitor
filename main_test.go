package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestProcessControlCodes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello", "hello"},
		{"carriage return overwrites", "old text\rnew text", "new text"},
		{"multiple CRs keep last", "a\rb\rc", "c"},
		{"backspace deletes char", "abc\bd", "abd"},
		{"multiple backspaces", "abcde\b\b\bxy", "abxy"},
		{"backspace at start is no-op", "\b\bhello", "hello"},
		{"CR then backspace", "old\rnew\bx", "nex"},
		{"no control codes", "just normal text", "just normal text"},
		{"progress bar", "[====      ] 40%\r[========  ] 80%\r[==========] 100%", "[==========] 100%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processControlCodes(tt.in)
			if got != tt.want {
				t.Errorf("processControlCodes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRingBufferCROverwrite(t *testing.T) {
	buf := NewRingBuffer(100)

	// Simulate a progress bar updating in place across multiple PTY reads
	buf.Write("Downloading... 10%")    // partial, no \n
	got := buf.Last(10)
	if len(got) != 1 || got[0] != "Downloading... 10%" {
		t.Fatalf("partial not shown: %v", got)
	}

	buf.Write("\rDownloading... 50%")   // CR overwrites partial
	got = buf.Last(10)
	if len(got) != 1 || got[0] != "Downloading... 50%" {
		t.Fatalf("CR overwrite failed: %v", got)
	}

	buf.Write("\rDownloading... 100%\n") // CR + newline commits line
	got = buf.Last(10)
	if len(got) != 1 || got[0] != "Downloading... 100%" {
		t.Fatalf("final line wrong: %v", got)
	}
}

func TestRingBufferMultiline(t *testing.T) {
	buf := NewRingBuffer(100)
	buf.Write("line1\nline2\nline3\n")

	got := buf.Last(3)
	want := []string{"line1", "line2", "line3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Last(3) = %v, want %v", got, want)
	}
}

func TestRingBufferCRLF(t *testing.T) {
	buf := NewRingBuffer(100)
	buf.Write("line1\r\nline2\r\n")

	got := buf.Last(2)
	want := []string{"line1", "line2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CRLF handling: got %v, want %v", got, want)
	}
}

func TestRingBufferPartialAcrossChunks(t *testing.T) {
	buf := NewRingBuffer(100)
	buf.Write("hello ")
	buf.Write("world\n")

	got := buf.Last(1)
	want := []string{"hello world"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("partial across chunks: got %v, want %v", got, want)
	}
}

func TestRingBufferANSIStrippedButCRPreserved(t *testing.T) {
	buf := NewRingBuffer(100)
	// Simulate: colored "old" text, then CR, then colored "new" text
	buf.Write("\x1b[31mold text\x1b[0m\r\x1b[32mnew text\x1b[0m\n")

	got := buf.Last(1)
	want := []string{"new text"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ANSI+CR: got %v, want %v", got, want)
	}
}

func TestStripANSIPreservesCR(t *testing.T) {
	input := "\x1b[2Kold\rnew"
	got := stripANSI(input)
	want := "old\rnew"
	if got != want {
		t.Errorf("stripANSI(%q) = %q, want %q", input, got, want)
	}
}

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
		Name:        "test-fox",
		Dir:         "/tmp/test",
		Flags:       []string{"--flag1"},
		Status:      "stopped",
		StartedAt:   time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		ResumeID:    "test-fox",
		output:      NewRingBuffer(500),
		rawOutput:   rawBuf,
		broadcaster: NewBroadcaster(),
	}
	sessionOrder = []string{"test-fox"}

	// Save
	records := buildSessionRecords()
	writeSessionsToDisk(records)
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

func TestCLIOverridesConfig(t *testing.T) {
	origConfig := config
	defer func() { config = origConfig }()

	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.json")
	os.WriteFile(cfgFile, []byte(`{"port": 9999, "allowed_dirs": ["~/old"]}`), 0644)

	config = Config{}
	loadConfigFrom(cfgFile)
	if config.Port != 9999 {
		t.Errorf("port = %d, want 9999", config.Port)
	}
	if len(config.AllowedDirs) != 1 || config.AllowedDirs[0] != "~/old" {
		t.Errorf("allowed_dirs = %v, want [~/old]", config.AllowedDirs)
	}

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
