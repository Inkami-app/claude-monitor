package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// detectTailscaleHostname returns the machine's Tailscale FQDN (without
// trailing dot), or an error if Tailscale is not available.
func detectTailscaleHostname() (string, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale not available: %w", err)
	}
	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("parsing tailscale status: %w", err)
	}
	hostname := strings.TrimSuffix(status.Self.DNSName, ".")
	if hostname == "" {
		return "", fmt.Errorf("tailscale returned empty hostname")
	}
	return hostname, nil
}

// generateTailscaleCerts runs `tailscale cert` to produce cert and key files
// in the given directory and returns the file paths.
func generateTailscaleCerts(hostname, dir string) (certFile, keyFile string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("creating cert directory: %w", err)
	}
	certFile = filepath.Join(dir, hostname+".crt")
	keyFile = filepath.Join(dir, hostname+".key")
	cmd := exec.Command("tailscale", "cert",
		"--cert-file", certFile,
		"--key-file", keyFile,
		hostname,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("tailscale cert failed: %w", err)
	}
	return certFile, keyFile, nil
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
		wantTLS         bool
		certFileInput   string
		keyFileInput    string
	)

	flagOptions := []huh.Option[string]{
		huh.NewOption("--dangerously-skip-permissions", "--dangerously-skip-permissions"),
		huh.NewOption("--chrome", "--chrome"),
		huh.NewOption("--verbose", "--verbose"),
	}

	// Detect Tailscale before building the form so we can customize the
	// TLS question with the detected hostname.
	tsHostname, tsErr := detectTailscaleHostname()
	hasTailscale := tsErr == nil && tsHostname != ""

	tlsDescription := "Required for HTTPS — needs Tailscale"
	if hasTailscale {
		tlsDescription = fmt.Sprintf("Detected: %s — will generate certs automatically", tsHostname)
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

		huh.NewGroup(
			huh.NewConfirm().
				Title("Set up TLS with Tailscale certs?").
				Description(tlsDescription).
				Value(&wantTLS),
		),

		// Manual cert path inputs — only shown when Tailscale is NOT
		// available and the user still wants TLS.
		huh.NewGroup(
			huh.NewInput().
				Title("Certificate file path").
				Description("Path to the .crt file").
				Placeholder("/path/to/hostname.crt").
				Value(&certFileInput),

			huh.NewInput().
				Title("Key file path").
				Description("Path to the .key file").
				Placeholder("/path/to/hostname.key").
				Value(&keyFileInput),
		).WithHideFunc(func() bool { return !wantTLS || hasTailscale }),
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

	// TLS certs
	if wantTLS {
		if hasTailscale {
			// Auto-generate certs into the config directory.
			home, _ := os.UserHomeDir()
			certDir := filepath.Join(home, ".config", "claude-monitor")
			fmt.Println()
			fmt.Printf("Generating Tailscale certs for %s...\n", tsHostname)
			certFile, keyFile, err := generateTailscaleCerts(tsHostname, certDir)
			if err != nil {
				fmt.Printf("Warning: %v\n", err)
				fmt.Println("You can configure certs manually in the config file later.")
			} else {
				cfg.CertFile = certFile
				cfg.KeyFile = keyFile

				successStyle := lipgloss.NewStyle().
					Foreground(lipgloss.Color("#10B981"))
				fmt.Println(successStyle.Render("TLS certs generated successfully!"))
			}
		} else if certFileInput != "" && keyFileInput != "" {
			cfg.CertFile = certFileInput
			cfg.KeyFile = keyFileInput
		}
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
