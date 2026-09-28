package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// geminiBackend implements Backend by spawning the Google Gemini CLI
// with `--output-format stream-json` and parsing its NDJSON event stream.
type geminiBackend struct {
	cfg Config
}

func (b *geminiBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "gemini"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("gemini executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildGeminiArgs(prompt, opts, b.cfg.Logger)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildGeminiEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gemini stdout pipe: %w", err)
	}
	cmd.Stderr = newLogWriter(b.cfg.Logger, "[gemini:stderr] ")

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start gemini: %w", err)
	}

	b.cfg.Logger.Info("gemini started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// Close stdout when the context is cancelled so scanner.Scan() unblocks.
	go func() {
		<-runCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		var output strings.Builder
		var sessionID string
		finalStatus := "completed"
		var finalError string
		usage := make(map[string]TokenUsage)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var evt geminiStreamEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}

			switch evt.Type {
			case "init":
				sessionID = evt.SessionID
				trySend(msgCh, Message{Type: MessageStatus, Status: "running"})

			case "message":
				if evt.Role == "assistant" && evt.Content != "" {
					output.WriteString(evt.Content)
					trySend(msgCh, Message{Type: MessageText, Content: evt.Content})
				}

			case "tool_use":
				var params map[string]any
				if evt.Parameters != nil {
					_ = json.Unmarshal(evt.Parameters, &params)
				}
				trySend(msgCh, Message{
					Type:   MessageToolUse,
					Tool:   evt.ToolName,
					CallID: evt.ToolID,
					Input:  params,
				})

			case "tool_result":
				trySend(msgCh, Message{
					Type:   MessageToolResult,
					CallID: evt.ToolID,
					Output: evt.Output,
				})

			case "error":
				trySend(msgCh, Message{
					Type:    MessageError,
					Content: evt.Message,
				})

			case "result":
				if evt.Status == "error" && evt.Error != nil {
					finalStatus = "failed"
					finalError = evt.Error.Message
				}
				if evt.Stats != nil {
					b.accumulateUsage(usage, evt.Stats)
				}
			}
		}

		waitErr := cmd.Wait()
		duration := time.Since(startTime)

		if runCtx.Err() == context.DeadlineExceeded {
			finalStatus = "timeout"
			finalError = fmt.Sprintf("gemini timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			finalStatus = "aborted"
			finalError = "execution cancelled"
		} else if waitErr != nil && finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("gemini exited with error: %v", waitErr)
		}

		b.cfg.Logger.Info("gemini finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		resCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// accumulateUsage extracts per-model token usage from Gemini's result stats.
func (b *geminiBackend) accumulateUsage(usage map[string]TokenUsage, stats *geminiStreamStats) {
	for model, m := range stats.Models {
		u := usage[model]
		u.InputTokens += int64(m.InputTokens)
		u.OutputTokens += int64(m.OutputTokens)
		u.CacheReadTokens += int64(m.Cached)
		usage[model] = u
	}
}

// ── Gemini stream-json event types ──

type geminiStreamEvent struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`

	// message fields
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Delta   bool   `json:"delta,omitempty"`

	// tool_use fields
	ToolName   string          `json:"tool_name,omitempty"`
	ToolID     string          `json:"tool_id,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`

	// tool_result fields
	Status string `json:"status,omitempty"`
	Output string `json:"output,omitempty"`

	// error fields
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message,omitempty"`

	// result fields
	Error *geminiStreamError `json:"error,omitempty"`
	Stats *geminiStreamStats `json:"stats,omitempty"`
}

type geminiStreamError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

type geminiStreamStats struct {
	Models map[string]geminiStreamModelStats `json:"models,omitempty"`
}

type geminiStreamModelStats struct {
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
	Cached       int `json:"cachedContentReadTokens,omitempty"`
}

// buildGeminiArgs constructs the argv for a Gemini CLI invocation.
// It uses `--yolo` for non-interactive approval and `--output-format stream-json`
// for the NDJSON event stream.
func buildGeminiArgs(prompt string, opts ExecOptions, logger *slog.Logger) []string {
	var args []string
	args = append(args, "-p", prompt)
	args = append(args, "--yolo")
	args = append(args, "-o", "stream-json")

	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "-r", opts.ResumeSessionID)
	}
	if len(opts.CustomArgs) > 0 {
		args = append(args, opts.CustomArgs...)
	}
	return args
}

// buildGeminiEnv constructs the environment for a Gemini CLI subprocess.
// GEMINI_CLI_TRUST_WORKSPACE=true is required for headless mode to skip
// the interactive folder-trust gate.
func buildGeminiEnv(extra map[string]string) []string {
	var env []string
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	// Set the trust workspace flag for headless mode.
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "GEMINI_CLI_TRUST_WORKSPACE=") {
			found = true
			break
		}
	}
	if !found {
		env = append(env, "GEMINI_CLI_TRUST_WORKSPACE=true")
	}
	return env
}
