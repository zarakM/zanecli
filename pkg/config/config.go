package config

// First-run wizard + persisted user config under ~/.zane/config.json.
// Env vars (ANTHROPIC_API_KEY, KUBECONFIG, SUPABASE_URL/KEY) take precedence
// over the saved file on Load — same precedence story we used in kubectl-ai.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config is everything zane persists between launches.
// File mode is 0600 — it contains an API key.
type Config struct {
	AnthropicAPIKey  string `json:"anthropic_api_key"`
	KubeconfigPath   string `json:"kubeconfig_path"`
	TelemetryEnabled bool   `json:"telemetry_enabled"`
	// Supabase destination for telemetry. Empty = telemetry silently no-ops
	// even when TelemetryEnabled is true. Env (SUPABASE_URL/KEY) and ldflags
	// still override these at the telemetry layer; see pkg/telemetry.
	SupabaseURL    string `json:"supabase_url,omitempty"`
	SupabaseKey    string `json:"supabase_key,omitempty"`
	HistoryEnabled bool   `json:"history_enabled"`
	// AutoExec opts the session into auto-executing whitelisted writes
	// (delete_pod, restart_deployment) when the safety guard's other
	// preconditions pass. Default: false. Override per-invocation with
	// --auto / --no-auto, or mid-session with /auto and /no-auto.
	AutoExec bool `json:"auto_exec"`
}

// Paths returns the config directory and file path. Created on first save.
func Paths() (dir, file string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("could not resolve home dir: %w", err)
	}
	dir = filepath.Join(home, ".zane")
	file = filepath.Join(dir, "config.json")
	return dir, file, nil
}

// Load reads the config file and applies env-var overrides. Returns
// (cfg, exists, err) — exists=false means the wizard should run.
func Load() (*Config, bool, error) {
	_, file, err := Paths()
	if err != nil {
		return nil, false, err
	}

	data, err := os.ReadFile(file)
	if errIsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("could not read %s: %w", file, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, true, fmt.Errorf("malformed config at %s: %w", file, err)
	}

	cfg.applyEnvOverrides()
	return &cfg, true, nil
}

// applyEnvOverrides lets env vars beat the file. Mirrors the previous
// kubectl-ai precedence (env > ldflags / file > defaults).
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		c.AnthropicAPIKey = v
	}
	if v := os.Getenv("KUBECONFIG"); v != "" {
		c.KubeconfigPath = v
	}
}

// Save writes the config to ~/.zane/config.json with 0600 perms.
func (c *Config) Save() error {
	dir, file, err := Paths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return fmt.Errorf("could not write %s: %w", file, err)
	}
	return nil
}

// RunWizard prompts the user for required fields and saves the config.
// Returns the populated Config. Reads from in (typically os.Stdin) and
// writes prompts to out (typically os.Stdout) so it's testable.
func RunWizard(in io.Reader, out io.Writer) (*Config, error) {
	scanner := bufio.NewScanner(in)
	read := func() string {
		if !scanner.Scan() {
			return ""
		}
		return strings.TrimSpace(scanner.Text())
	}

	fmt.Fprintln(out, "First-run setup. We need a few details — written to ~/.zane/config.json.")
	fmt.Fprintln(out)

	cfg := &Config{}

	// Anthropic API key — accept env-var fallback if set.
	if env := os.Getenv("ANTHROPIC_API_KEY"); env != "" {
		fmt.Fprintf(out, "Anthropic API key found in ANTHROPIC_API_KEY env var. Use it? [Y/n]: ")
		if !asksNo(read()) {
			cfg.AnthropicAPIKey = env
		}
	}
	for cfg.AnthropicAPIKey == "" {
		fmt.Fprint(out, "Anthropic API key (starts with 'sk-ant-'): ")
		v := read()
		if v == "" {
			fmt.Fprintln(out, "  required.")
			continue
		}
		cfg.AnthropicAPIKey = v
	}

	// Kubeconfig path — autodetect ~/.kube/config; let the user override.
	defaultKube := defaultKubeconfigPath()
	if defaultKube != "" && fileExists(defaultKube) {
		fmt.Fprintf(out, "Kubeconfig at %s detected. Use it? [Y/n]: ", defaultKube)
		if !asksNo(read()) {
			cfg.KubeconfigPath = defaultKube
		}
	}
	for cfg.KubeconfigPath == "" {
		fmt.Fprint(out, "Path to kubeconfig: ")
		v := read()
		if v == "" {
			fmt.Fprintln(out, "  required.")
			continue
		}
		if !fileExists(expandHome(v)) {
			fmt.Fprintf(out, "  %s does not exist; please double-check.\n", v)
			continue
		}
		cfg.KubeconfigPath = expandHome(v)
	}

	// Telemetry — default ON. Explained briefly.
	fmt.Fprint(out, "Send anonymous error-type telemetry? (no pod names / env values stored) [Y/n]: ")
	cfg.TelemetryEnabled = !asksNo(read())

	// Supabase destination — only asked when telemetry is on. Optional:
	// blank leaves telemetry a silent no-op (env/ldflags can still supply it
	// later). Env vars are offered as the default so a shell-exported value
	// doesn't have to be retyped.
	if cfg.TelemetryEnabled {
		if env := os.Getenv("SUPABASE_URL"); env != "" {
			fmt.Fprintf(out, "Supabase URL found in SUPABASE_URL env var. Use it? [Y/n]: ")
			if !asksNo(read()) {
				cfg.SupabaseURL = env
				cfg.SupabaseKey = os.Getenv("SUPABASE_KEY")
			}
		}
		if cfg.SupabaseURL == "" {
			fmt.Fprint(out, "Supabase project URL for telemetry (blank to skip): ")
			cfg.SupabaseURL = read()
		}
		if cfg.SupabaseURL != "" && cfg.SupabaseKey == "" {
			fmt.Fprint(out, "Supabase anon/service key: ")
			cfg.SupabaseKey = read()
		}
		if cfg.SupabaseURL == "" || cfg.SupabaseKey == "" {
			fmt.Fprintln(out, "  (no Supabase credentials — telemetry stays off until SUPABASE_URL/KEY are set)")
		}
	}

	// History — default OFF. Privacy-first: it stores resource names locally.
	fmt.Fprint(out, "Persist conversation history locally? It includes resource names from your cluster, never uploaded. [y/N]: ")
	cfg.HistoryEnabled = asksYes(read())

	// MVP: auto-exec is forced off in main.go. Skip the wizard prompt so
	// new users don't see a control that has no effect.
	cfg.AutoExec = false

	if err := cfg.Save(); err != nil {
		return nil, err
	}

	dir, _, _ := Paths()
	fmt.Fprintf(out, "\n✓ Saved to %s/config.json\n\n", dir)
	return cfg, nil
}

// LoadOrWizard is the convenience entry point: returns a usable config,
// running the wizard interactively on first launch.
func LoadOrWizard(in io.Reader, out io.Writer) (*Config, error) {
	cfg, exists, err := Load()
	if err != nil {
		return nil, err
	}
	if exists {
		return cfg, nil
	}
	return RunWizard(in, out)
}

// --- helpers ---

func errIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

func asksYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

func asksNo(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "n" || s == "no"
}

func defaultKubeconfigPath() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
