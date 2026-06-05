package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("ZANE_TELEMETRY", "")

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

	dir := filepath.Join(home, ".zane")
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

// The wizard must never ask the user about telemetry or Supabase: telemetry is
// on by default and its destination is baked in at build time. It should print a
// transparency note pointing at the DO_NOT_TRACK off switch instead.
func TestRunWizard_NoTelemetryPrompts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("ZANE_TELEMETRY", "")

	// A kubeconfig the wizard can autodetect and accept.
	kube := filepath.Join(home, "kubeconfig")
	if err := os.WriteFile(kube, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("KUBECONFIG", kube)

	// Answers: accept the env API key, accept the detected kubeconfig, decline
	// history. Three prompts, three blank lines — no telemetry prompt at all.
	var out bytes.Buffer
	cfg, err := RunWizard(strings.NewReader("\n\n\n"), &out)
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}

	if !cfg.TelemetryEnabled {
		t.Error("telemetry should default on")
	}
	// Match the actual old prompt phrasings, not a bare "supabase" substring —
	// the temp-dir path can contain the test name.
	lower := strings.ToLower(out.String())
	for _, banned := range []string{"supabase url", "supabase project", "supabase anon", "send anonymous error-type telemetry"} {
		if strings.Contains(lower, banned) {
			t.Errorf("wizard must not prompt for telemetry/Supabase (found %q), got:\n%s", banned, out.String())
		}
	}
	if !strings.Contains(out.String(), "DO_NOT_TRACK") {
		t.Error("wizard should surface the DO_NOT_TRACK off switch")
	}
}

// DO_NOT_TRACK (and the ZANE_TELEMETRY kill switch) force telemetry off at load
// time, beating a saved telemetry_enabled:true.
func TestLoad_EnvDisablesTelemetry(t *testing.T) {
	for _, tc := range []struct{ envKey, envVal string }{
		{"DO_NOT_TRACK", "1"},
		{"ZANE_TELEMETRY", "off"},
	} {
		t.Run(tc.envKey+"="+tc.envVal, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("DO_NOT_TRACK", "")
			t.Setenv("ZANE_TELEMETRY", "")

			saved := &Config{AnthropicAPIKey: "k", KubeconfigPath: "/kc", TelemetryEnabled: true}
			if err := saved.Save(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.envKey, tc.envVal)

			got, _, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got.TelemetryEnabled {
				t.Errorf("%s=%s should force telemetry off", tc.envKey, tc.envVal)
			}
		})
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
