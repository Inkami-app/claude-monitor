package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// needsSetup returns true when no config file exists on disk.
func needsSetup() bool {
	_, err := os.Stat(configPath())
	return os.IsNotExist(err)
}

// isTerminal returns true when stdin is attached to a terminal (char device).
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// runSetupWizard presents an interactive first-run configuration form and
// returns the resulting Config. The caller is responsible for persisting it.
func runSetupWizard() (*Config, error) {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#7C3AED")).
		MarginBottom(1)

	fmt.Println(titleStyle.Render("Welcome to claude-monitor setup!"))
	fmt.Println("Let's configure your environment.")
	fmt.Println()

	var (
		dirsInput       string
		selectedFlags   []string
		extraFlagsInput string
		portInput       string
		wantAuth        bool
	)

	flagOptions := []huh.Option[string]{
		huh.NewOption("--dangerously-skip-permissions", "--dangerously-skip-permissions"),
		huh.NewOption("--chrome", "--chrome"),
		huh.NewOption("--remote-control", "--remote-control"),
		huh.NewOption("--verbose", "--verbose"),
	}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Allowed directories").
				Description("Comma-separated list of directories where Claude can work").
				Placeholder("~/src, ~/projects").
				Value(&dirsInput),

			huh.NewMultiSelect[string]().
				Title("Claude CLI flags").
				Description("Select common flags to pass to the Claude CLI").
				Options(flagOptions...).
				Value(&selectedFlags),

			huh.NewInput().
				Title("Additional Claude flags").
				Description("Any extra flags (space-separated)").
				Placeholder("--model opus").
				Value(&extraFlagsInput),

			huh.NewInput().
				Title("Port").
				Description("HTTP port for the web UI").
				Placeholder("7777").
				Value(&portInput).
				Validate(func(s string) error {
					if s == "" {
						return nil // will default to 7777
					}
					n, err := strconv.Atoi(s)
					if err != nil {
						return fmt.Errorf("port must be a number")
					}
					if n < 1 || n > 65535 {
						return fmt.Errorf("port must be between 1 and 65535")
					}
					return nil
				}),

			huh.NewConfirm().
				Title("Set up authentication?").
				Description("Generate a random auth token to protect the web UI").
				Value(&wantAuth),
		),
	)

	if err := form.Run(); err != nil {
		return nil, fmt.Errorf("setup wizard cancelled: %w", err)
	}

	// --- Build Config from form answers ---

	cfg := &Config{Port: 7777}

	// Directories
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

	// Claude flags: merge multi-select + free-text
	cfg.ClaudeFlags = append(cfg.ClaudeFlags, selectedFlags...)
	if extraFlagsInput != "" {
		for _, f := range strings.Fields(extraFlagsInput) {
			if f != "" {
				cfg.ClaudeFlags = append(cfg.ClaudeFlags, f)
			}
		}
	}

	// Port
	if portInput != "" {
		n, _ := strconv.Atoi(portInput) // already validated
		cfg.Port = n
	}

	// Auth token
	if wantAuth {
		token, err := generateToken()
		if err != nil {
			return nil, fmt.Errorf("generating auth token: %w", err)
		}
		cfg.AuthToken = token

		tokenStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#10B981"))
		fmt.Printf("\nYour auth token: %s\n", tokenStyle.Render(token))
		fmt.Println("Save this — you'll need it to access the web UI.")
	}

	return cfg, nil
}

// generateToken returns a cryptographically random 32-character hex string.
func generateToken() (string, error) {
	b := make([]byte, 16) // 16 bytes = 32 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
