package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Saved config must be mode 0600 — the comment at line 17 of config.go calls
// this out by name and the API key sits inside the file.
func TestSave_FileModeIs0600(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &Config{
		AnthropicAPIKey: "sk-ant-xxx",
		KubeconfigPath:  "/tmp/kc",
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	_, file, _ := Paths()
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600 (file contains the API key)", fi.Mode().Perm())
	}
}

func TestLoad_ExistsFalseWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, exists, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if exists || cfg != nil {
		t.Errorf("expected (nil, false, nil), got (%v, %v)", cfg, exists)
	}
}

func TestLoad_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := &Config{
		AnthropicAPIKey:  "sk-ant-saved",
		KubeconfigPath:   "/tmp/kc",
		TelemetryEnabled: true,
		HistoryEnabled:   false,
		AutoExec:         false,
	}
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}

	// Clear env so we know the value came from the file, not overrides.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBECONFIG", "")

	got, exists, err := Load()
	if err != nil || !exists {
		t.Fatalf("Load: exists=%v err=%v", exists, err)
	}
	if got.AnthropicAPIKey != want.AnthropicAPIKey {
		t.Errorf("api key: got=%q want=%q", got.AnthropicAPIKey, want.AnthropicAPIKey)
	}
	if got.KubeconfigPath != want.KubeconfigPath {
		t.Errorf("kubeconfig: got=%q want=%q", got.KubeconfigPath, want.KubeconfigPath)
	}
	if got.TelemetryEnabled != want.TelemetryEnabled {
		t.Errorf("telemetry: %v", got.TelemetryEnabled)
	}
}

// Env vars must beat the file. This precedence is the only way the prod
// build with ldflags-baked-but-overridable-via-env story works.
func TestLoad_EnvOverridesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	saved := &Config{AnthropicAPIKey: "from-file", KubeconfigPath: "/file/kc"}
	if err := saved.Save(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "from-env")
	t.Setenv("KUBECONFIG", "/env/kc")

	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AnthropicAPIKey != "from-env" {
		t.Errorf("env should override file for ANTHROPIC_API_KEY: %q", got.AnthropicAPIKey)
	}
	if got.KubeconfigPath != "/env/kc" {
		t.Errorf("env should override file for KUBECONFIG: %q", got.KubeconfigPath)
	}
}

func TestLoad_MalformedFileReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".zanecli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, exists, err := Load()
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
	if !exists {
		t.Error("exists should be true (file is present, just unparseable)")
	}
	if cfg != nil {
		t.Errorf("cfg should be nil on parse error, got %+v", cfg)
	}
}

// asksYes / asksNo helpers — small but they gate the wizard's flow.
func TestAsksYesNo(t *testing.T) {
	yeses := []string{"y", "Y", "yes", "YES", "  yes  "}
	for _, s := range yeses {
		if !asksYes(s) {
			t.Errorf("asksYes(%q) = false", s)
		}
	}
	if asksYes("") || asksYes("no") || asksYes("maybe") {
		t.Error("asksYes accepted non-yes input")
	}

	nos := []string{"n", "N", "no", "NO", "  no  "}
	for _, s := range nos {
		if !asksNo(s) {
			t.Errorf("asksNo(%q) = false", s)
		}
	}
	if asksNo("") || asksNo("yes") {
		t.Error("asksNo accepted non-no input")
	}
}
