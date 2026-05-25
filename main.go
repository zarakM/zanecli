package main

// zanecli — conversational Kubernetes co-pilot.
//
// Phase 5 entry point: REPL with config wizard, agent loop, write tools
// gated by pkg/safety, and optional persistent conversation history.
// Phase 6 polish (ANSI colors, error handling) is the remaining work.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/zarakM/zanecli/pkg/agent"
	"github.com/zarakM/zanecli/pkg/config"
	"github.com/zarakM/zanecli/pkg/history"
	"github.com/zarakM/zanecli/pkg/k8s"
	"github.com/zarakM/zanecli/pkg/telemetry"
	"github.com/zarakM/zanecli/pkg/tools"
	"github.com/zarakM/zanecli/pkg/ui"
)

// ClientVersion is injected at build time via -ldflags
// (e.g. -X main.ClientVersion=$(git rev-parse --short HEAD)). It identifies
// which client cut produced a row in the sessions table.
var ClientVersion = "dev"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap ⌃C: first signal cancels the in-flight agent step (if any);
	// second signal exits.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "\n(interrupting; press ⌃C again to exit)")
		cancel()
		<-sigs
		os.Exit(130)
	}()

	cfg, err := config.LoadOrWizard(os.Stdin, os.Stdout)
	if err != nil {
		fatalf("config error: %v\n\nIf the file at ~/.zanecli/config.json is corrupt, delete it and re-run.", err)
	}

	// MVP: auto-exec is disabled. Every write falls through to a y/N prompt.
	// We still honor any value loaded from older config files by forcing it
	// off here so a saved `auto_exec: true` cannot bypass confirmation.
	cfg.AutoExec = false

	// Hand the config-file Supabase credentials to the telemetry layer.
	// Env vars / ldflags still take precedence inside pkg/telemetry.
	telemetry.SetSupabaseConfig(cfg.SupabaseURL, cfg.SupabaseKey)

	client, err := k8s.NewClient(cfg.KubeconfigPath)
	if err != nil {
		fatalf("could not connect to cluster: %v\n\nIs your kubeconfig at %s valid? Try: kubectl --kubeconfig %s get pods", err, cfg.KubeconfigPath, cfg.KubeconfigPath)
	}

	// Single shared scanner for both REPL input and write-confirmation
	// prompts — they must not compete for stdin.
	scanner := bufio.NewScanner(os.Stdin)
	confirmer := &stdinConfirmer{scanner: scanner}

	registry := tools.NewRegistry(client)
	sess := agent.NewSession(cfg, client, registry, confirmer, ClientVersion)
	// Session implements tools.DiagnosticSink — wire it so diagnose_pod
	// and diagnose_rollout can hand back their structured payloads for
	// end-of-Step Supabase logging.
	registry.SetDiagnosticSink(sess)

	// History is opt-in. Open the writer only if the user enabled it.
	var writer *history.Writer
	if cfg.HistoryEnabled {
		writer, err = history.OpenWriter()
		if err != nil {
			fmt.Fprintf(os.Stderr, "history disabled: %v\n", err)
			writer = nil
		}
		if writer != nil {
			defer writer.Close()
		}
	}

	fmt.Printf("%szanecli%s — your Kubernetes co-pilot\n", ui.Bold+ui.Cyan, ui.Reset)
	fmt.Printf("Cluster: %s%s%s\n", ui.Dim, abbreviateServerURL(client.ServerURL()), ui.Reset)
	fmt.Printf("%s[every cluster change asks first — confirm with y/N]%s\n", ui.Dim, ui.Reset)

	// Offer to resume a prior session if history is on and a previous file exists.
	if cfg.HistoryEnabled {
		offerResume(sess, scanner)
	}

	fmt.Println("Type your question, or 'exit' to quit. /clear resets the chat; /good and /bad label the last answer.")
	fmt.Println()

	// Track the persisted prefix so we only append new messages after each Step.
	persistedPrefix := len(sess.Messages())

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "exit", "quit":
			return
		case "/clear":
			sess.Clear()
			persistedPrefix = 0
			fmt.Println("(conversation cleared)")
			continue
		case "/good":
			if sess.MarkFeedback(+1) {
				fmt.Println("(thanks — logged as 👍)")
			} else {
				fmt.Println("(no prior answer to label yet)")
			}
			continue
		case "/bad":
			if sess.MarkFeedback(-1) {
				fmt.Println("(thanks — logged as 👎; we'll use it to improve)")
			} else {
				fmt.Println("(no prior answer to label yet)")
			}
			continue
		}

		if err := sess.Step(ctx, line, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "%sagent error:%s %v\n", ui.Red, ui.Reset, err)
		}
		fmt.Println()

		// Persist any new messages this Step produced (user input + assistant
		// turns + tool results). Best-effort; a write failure doesn't break the chat.
		if writer != nil {
			msgs := sess.Messages()
			for i := persistedPrefix; i < len(msgs); i++ {
				if werr := writer.Append(msgs[i]); werr != nil {
					fmt.Fprintf(os.Stderr, "history write failed: %v\n", werr)
					break
				}
			}
			persistedPrefix = len(msgs)
		}
	}
}

// offerResume looks for the most recent prior session and asks the user if
// they want to load it as context. Always non-blocking: any error or "no"
// answer drops through to a fresh session.
func offerResume(sess *agent.Session, scanner *bufio.Scanner) {
	prior, err := history.LoadLatest()
	if err != nil || prior == nil || len(prior.Messages) == 0 {
		return
	}
	fmt.Printf("Resume last session (%s)? [y/N]: ", prior.Summary())
	if !scanner.Scan() {
		return
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	if answer == "y" || answer == "yes" {
		sess.LoadMessages(prior.Messages)
		fmt.Printf("Resumed %d messages from %s\n", len(prior.Messages), prior.Path)
	}
}

// stdinConfirmer asks yes/no on the shared REPL scanner. Lives here in main
// so the agent package stays free of stdin/terminal concerns.
type stdinConfirmer struct {
	scanner *bufio.Scanner
}

func (c *stdinConfirmer) AskYesNo(prompt string) bool {
	fmt.Print(prompt)
	if !c.scanner.Scan() {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(c.scanner.Text()))
	return s == "y" || s == "yes"
}

// fatalf prints a colored error and exits with code 1. Used at startup
// when there's nothing usable to fall back to.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s✗%s "+format+"\n", append([]any{ui.Red, ui.Reset}, args...)...)
	os.Exit(1)
}

// abbreviateServerURL turns "https://prod-east.cluster.local:6443" into
// something short enough for the banner without leaking full URLs.
func abbreviateServerURL(s string) string {
	if s == "" {
		return "(no API server resolved)"
	}
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}
