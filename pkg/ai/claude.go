package ai

// Anthropic API plumbing. We call /v1/messages directly over HTTP — no SDK,
// no LLM framework. streamTo is for streaming SSE responses (the agent's
// turn-level output); complete is for one-shot non-streaming JSON answers.
//
// This file deliberately holds no domain logic. Phase 3 layers the agent
// loop, tools, and system prompts on top of this transport.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	claudeAPIURL = "https://api.anthropic.com/v1/messages"
	claudeModel  = "claude-sonnet-4-20250514"
)

// ClaudeClient wraps the API key and a reusable HTTP client.
// http.Client is safe for concurrent use and should be reused, not created per request.
type ClaudeClient struct {
	apiKey     string
	httpClient *http.Client
}

func NewClaudeClient(apiKey string) *ClaudeClient {
	return &ClaudeClient{
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

// Model returns the Claude model id this client targets. Exposed so callers
// (telemetry, logs) can record which model produced a response.
func (c *ClaudeClient) Model() string {
	return claudeModel
}

// claudeRequest mirrors the Anthropic /v1/messages JSON body.
// omitempty lets us share this struct between streaming and non-streaming calls
// (Stream=false in non-streaming mode).
type claudeRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Stream    bool      `json:"stream"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// streamEvent mirrors the SSE JSON payloads the streaming API emits.
// Only the fields we act on are decoded; unknown fields are silently ignored.
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"` // "text_delta" for token chunks
		Text string `json:"text"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// completeResponse mirrors the non-streaming /v1/messages body.
type completeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// streamTo sends a streaming request and writes text-delta tokens to out as
// they arrive. SSE format: payload lines start with "data: "; blank lines
// separate events. We decode each chunk and forward immediately so the user
// sees output token by token. Phase 3 will wrap this with an agent loop that
// also intercepts tool_use blocks; for now this is the bare transport.
func (c *ClaudeClient) streamTo(ctx context.Context, reqBody claudeRequest, out io.Writer) error {
	reqBody.Stream = true

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				fmt.Fprint(out, event.Delta.Text)
			}
		case "error":
			return fmt.Errorf("Claude API error (%s): %s", event.Error.Type, event.Error.Message)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading stream: %w", err)
	}
	fmt.Fprintln(out)
	return nil
}

// complete is the non-streaming companion. Used when the caller wants the full
// assistant text as a string (e.g. small JSON answers from a structured prompt).
func (c *ClaudeClient) complete(ctx context.Context, reqBody claudeRequest) (string, error) {
	reqBody.Stream = false

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed completeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("could not decode response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("Claude API error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}

	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// Agent turn types — used by pkg/agent for Anthropic tool use.
// These mirror the /v1/messages JSON shape with content blocks: every
// assistant message's content is a list of blocks (text, tool_use), and
// every user message that contains tool results is also a list of blocks.
// ---------------------------------------------------------------------------

// AgentRequest is the body of a tool-use turn. The first user message with
// the user's free-form question is added by the agent; each subsequent
// user message carries tool_result blocks for the prior assistant turn.
type AgentRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []AgentMessage `json:"messages"`
	Tools     []ToolDef      `json:"tools,omitempty"`
}

// AgentMessage carries a list of content blocks. The Anthropic API allows
// a user message's content to be a string OR an array of blocks; we always
// use the array form for uniformity.
type AgentMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is the union of "text", "tool_use", and "tool_result" shapes
// the API accepts in a content array. Fields marked omitempty so we can
// reuse one struct for all three block types.
type ContentBlock struct {
	Type string `json:"type"`

	// "text" blocks
	Text string `json:"text,omitempty"`

	// "tool_use" blocks (assistant)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// "tool_result" blocks (user, follow-up to a tool_use)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ToolDef declares a tool the agent may call. The schema is plain JSON —
// each tool implementation supplies its own. See pkg/tools/<tool>.go.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// AgentResponse mirrors the /v1/messages non-streaming body when tools
// are in use. StopReason is "tool_use" when the assistant wants more data,
// "end_turn" when it has produced a final answer.
type AgentResponse struct {
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// AgentTurn POSTs a tool-use request and returns the parsed response.
// Non-streaming for v1 — tool-use streaming is more complex (need to
// accumulate text_delta and input_json_delta across content_block_delta
// events) and the UX gain is small for short tool chains.
func (c *ClaudeClient) AgentTurn(ctx context.Context, req AgentRequest) (*AgentResponse, error) {
	if req.Model == "" {
		req.Model = claudeModel
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 2048
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed AgentResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("could not decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("Claude API error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}
	return &parsed, nil
}
