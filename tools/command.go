package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const defaultCommandTimeout = 30 * time.Second
const maxCommandOutputBytes = 1 << 20

type commandOutput struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

type runCommandArgs struct {
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	TimeoutMS int      `json:"timeout_ms"`
}

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

// bashTool runs a full bash command line directly in the workspace so the
// model can type shell the way a human would (CHANGE-024, REQ-035).
func bashTool(rootAt func(context.Context) string) handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		workspace := rootAt(ctx)
		var args bashArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Command) == "" {
			return "", errors.New("command is required")
		}

		runCtx := ctx
		cancel := func() {}
		if args.TimeoutMS > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutMS)*time.Millisecond)
		} else {
			runCtx, cancel = context.WithTimeout(ctx, defaultCommandTimeout)
		}
		defer cancel()

		cmd := shellCommand(args.Command)
		cmd.Dir = workspace
		configureCommandProcess(cmd)

		var out limitedCommandBuffer
		cmd.Stdout, cmd.Stderr = &out, &out

		if err := cmd.Start(); err != nil {
			return "", err
		}

		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()

		var err error
		select {
		case err = <-waitCh:
		case <-runCtx.Done():
			killCommandProcessTree(cmd)
			err = <-waitCh
		}

		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		result := commandOutput{Output: out.String(), ExitCode: code}
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return "", marshalErr
		}
		if err != nil {
			if runCtx.Err() != nil {
				return string(data), fmt.Errorf("command %s: %w", args.Command, runCtx.Err())
			}
			return string(data), fmt.Errorf("command %s failed: %w", args.Command, err)
		}
		return string(data), nil
	}
}

type limitedCommandBuffer struct{ data []byte }

func (b *limitedCommandBuffer) Write(p []byte) (int, error) {
	if len(b.data) < maxCommandOutputBytes {
		n := len(p)
		if len(b.data)+n > maxCommandOutputBytes {
			n = maxCommandOutputBytes - len(b.data)
		}
		b.data = append(b.data, p[:n]...)
	}
	return len(p), nil
}

func (b *limitedCommandBuffer) String() string { return strings.TrimSpace(string(b.data)) }

// hasShellSyntax reports whether a bare command line relies on shell
// features (chains, pipes, redirects, expansions). Such lines run
// through the system shell; anything else executes directly.
func hasShellSyntax(line string) bool {
	if strings.Contains(line, "\n") {
		return true
	}
	for _, op := range []string{"&&", "||", "|", ";", ">", "<", "$", "`", "*", "?"} {
		if strings.Contains(line, op) {
			return true
		}
	}
	return false
}

func parsePositiveInt(value string, fallback int) int {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
