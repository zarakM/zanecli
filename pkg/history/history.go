package history

// Persistent conversation history. Each chat session writes a JSONL file
// under ~/.zanecli/history/<UTC-timestamp>.jsonl, one message per line.
// JSONL is chosen so a partial write during a crash leaves valid prior
// lines readable.
//
// History is OFF by default. The wizard explicitly opts the user in.
// Files are 0600 — they contain resource names from the user's cluster.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zarakM/zanecli/pkg/ai"
)

// HistoryDir returns the directory where session JSONLs live.
func HistoryDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".zanecli", "history"), nil
}

// Session is one persisted chat session.
type Session struct {
	Path      string            // absolute path to the JSONL file
	StartedAt time.Time         // parsed from filename
	Messages  []ai.AgentMessage // ordered, oldest-first
}

// Writer appends messages to a session file as they arrive. Open one per
// chat session in main.go; defer Close on exit.
type Writer struct {
	f    *os.File
	path string
}

// OpenWriter creates a new history file with a UTC ISO timestamp filename.
// File mode is 0600 — only the user can read it.
func OpenWriter() (*Writer, error) {
	dir, err := HistoryDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	// Filename uses dashes instead of colons so it's safe on every filesystem.
	name := time.Now().UTC().Format("2006-01-02T15-04-05Z") + ".jsonl"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, path: path}, nil
}

// Path returns the absolute file path. Useful for debug output.
func (w *Writer) Path() string { return w.path }

// Append writes one message as a single JSONL line.
func (w *Writer) Append(msg ai.AgentMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	_, err = w.f.Write([]byte{'\n'})
	return err
}

// Close finalizes the file.
func (w *Writer) Close() error {
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}

// LoadLatest returns the most recent session, or nil if nothing exists.
// Filenames are ISO timestamps so sort.StringSlice descending picks the newest.
func LoadLatest() (*Session, error) {
	dir, err := HistoryDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	return loadFile(filepath.Join(dir, names[0]))
}

func loadFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sess := &Session{Path: path}

	// Best-effort timestamp parse from filename. Used only for display.
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if t, perr := time.Parse("2006-01-02T15-04-05Z", base); perr == nil {
		sess.StartedAt = t
	}

	scanner := bufio.NewScanner(f)
	// Allow long lines — a single tool_result with logs can be 50KB+.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var msg ai.AgentMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			// Skip malformed lines — never fail to load a partly-written history.
			continue
		}
		sess.Messages = append(sess.Messages, msg)
	}
	return sess, scanner.Err()
}

// Summary returns a short one-line description of the session for the
// resume prompt: "Mar 5 14:22 — 12 messages".
func (s *Session) Summary() string {
	count := len(s.Messages)
	when := "unknown time"
	if !s.StartedAt.IsZero() {
		when = s.StartedAt.Local().Format("Jan 2 15:04")
	}
	return fmt.Sprintf("%s — %d messages", when, count)
}
