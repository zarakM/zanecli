package ui

// Tiny ANSI helper. Codes turn into empty strings when stdout is not a TTY,
// so piping zane output to a file produces clean text. No third-party
// dependency — we already use direct HTTP everywhere; one less package.

import "os"

var enabled = isTerminal()

// isTerminal reports whether stdout looks like an interactive terminal.
// File / pipe redirection clears the ModeCharDevice bit.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Color codes. Empty strings when not a TTY, so prefix/suffix concatenation
// is always safe — no NIL checks, no string format twists.
var (
	Reset  = code("\x1b[0m")
	Dim    = code("\x1b[2m")
	Bold   = code("\x1b[1m")
	Cyan   = code("\x1b[36m")
	Yellow = code("\x1b[33m")
	Red    = code("\x1b[31m")
	Green  = code("\x1b[32m")
)

func code(s string) string {
	if !enabled {
		return ""
	}
	return s
}
