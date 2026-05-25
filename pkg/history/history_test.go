package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zarakM/zanecli/pkg/ai"
)

// withTempHome sets HOME so HistoryDir() resolves under a t.TempDir.
// History writes to ~/.zanecli/history/...; isolating $HOME keeps the
// real home untouched and the test parallel-safe.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// On macOS, os.UserHomeDir reads $HOME; on other platforms USERPROFILE
	// may apply, but the Go runtime honours $HOME first when set.
	return dir
}

func TestOpenWriter_FileModeIs0600(t *testing.T) {
	home := withTempHome(t)
	w, err := OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	fi, err := os.Stat(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fi.Mode().Perm())
	}
	if !strings.HasPrefix(w.Path(), filepath.Join(home, ".zanecli", "history")) {
		t.Errorf("file path not under home: %s", w.Path())
	}
}

func TestWriter_AppendAndLoadLatest_Roundtrip(t *testing.T) {
	withTempHome(t)
	w, err := OpenWriter()
	if err != nil {
		t.Fatal(err)
	}
	msgs := []ai.AgentMessage{
		{Role: "user", Content: []ai.ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "hi"}}},
	}
	for _, m := range msgs {
		if err := w.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	sess, err := LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("LoadLatest returned nil")
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("loaded %d messages, want 2", len(sess.Messages))
	}
	if sess.Messages[0].Role != "user" || sess.Messages[1].Role != "assistant" {
		t.Errorf("roles round-tripped wrong: %+v", sess.Messages)
	}
	if sess.Messages[0].Content[0].Text != "hello" {
		t.Errorf("text round-tripped wrong: %q", sess.Messages[0].Content[0].Text)
	}
}

func TestLoadLatest_NoHistoryReturnsNil(t *testing.T) {
	withTempHome(t)
	sess, err := LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest with missing dir: %v", err)
	}
	if sess != nil {
		t.Errorf("expected nil session, got %+v", sess)
	}
}

// A corrupt JSONL line must be skipped, not abort the load — the comment in
// history.go (line 143) names this behavior explicitly.
func TestLoadFile_SkipsCorruptLines(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".zanecli", "history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-01-01T00-00-00Z.jsonl")

	good, _ := json.Marshal(ai.AgentMessage{Role: "user", Content: []ai.ContentBlock{{Type: "text", Text: "good"}}})
	contents := string(good) + "\n" + "not-json-at-all\n" + string(good) + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	sess, err := LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil || len(sess.Messages) != 2 {
		t.Errorf("want 2 good messages (corrupt skipped), got %+v", sess)
	}
}

// Multiple session files exist; LoadLatest must pick the newest by filename.
func TestLoadLatest_PicksNewestByName(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".zanecli", "history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		path := filepath.Join(dir, name)
		m := ai.AgentMessage{Role: "user", Content: []ai.ContentBlock{{Type: "text", Text: text}}}
		b, _ := json.Marshal(m)
		_ = os.WriteFile(path, append(b, '\n'), 0o600)
	}
	write("2026-01-01T00-00-00Z.jsonl", "older")
	write("2026-05-01T12-30-00Z.jsonl", "newer")

	sess, err := LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if sess.Messages[0].Content[0].Text != "newer" {
		t.Errorf("loaded older session; want newer")
	}
	if sess.StartedAt.IsZero() {
		t.Error("StartedAt should be parsed from filename")
	}
}

func TestSession_Summary(t *testing.T) {
	s := &Session{
		StartedAt: time.Date(2026, 3, 5, 14, 22, 0, 0, time.UTC),
		Messages:  make([]ai.AgentMessage, 12),
	}
	if !strings.Contains(s.Summary(), "12 messages") {
		t.Errorf("summary missing count: %q", s.Summary())
	}
}
