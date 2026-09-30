package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCommandStdinProbeHelper(t *testing.T) {
	if os.Getenv("GO_WANT_COMMAND_STDIN_PROBE") != "1" {
		return
	}
	if os.Getenv("GO_WANT_COMMAND_STDIN_EARLY_EXIT") == "1" {
		os.Exit(0)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "read stdin: %v", err)
		os.Exit(2)
	}
	sum := sha256.Sum256(data)
	_, _ = fmt.Fprintf(os.Stdout, "%d %s", len(data), hex.EncodeToString(sum[:]))
	os.Exit(0)
}

func stdinProbeCommand(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("& '%s' -test.run=TestCommandStdinProbeHelper", strings.ReplaceAll(executable, "'", "''"))
	}
	return fmt.Sprintf("%q -test.run=TestCommandStdinProbeHelper", executable)
}

func TestExecCommandDoesNotFilterCommandContent(t *testing.T) {
	rt, _ := newCommandTestService(t)
	command := `printf 'shell=%s network=%s\n' "$(printf expansion)" "https://example.test"`
	if runtime.GOOS == "windows" {
		command = `Write-Output "shell=expansion network=https://example.test"`
	}
	result, err := rt.execArgs(context.Background(), map[string]any{
		"cmd":            command,
		"yield_time_ms":  15000,
		"timeout_ms":     15000,
		"execution_mode": "sync",
	})
	if err != nil {
		t.Fatalf("exec_command should not reject command content: %v", err)
	}
	if result["status"] != "exited" || !strings.Contains(result["stdout"].(string), "shell=expansion network=https://example.test") {
		t.Fatalf("unexpected command result: %#v", result)
	}
}

func TestExecCommandForwardsConfiguredHostEnv(t *testing.T) {
	rt, cfg := newCommandTestService(t)
	t.Setenv("AGENTDOCK_TEST_HOST_PASSTHROUGH", "host-forwarded")
	cfg.CommandEnvFromEnv = map[string]string{
		"AGENTDOCK_TEST_CHILD_PASSTHROUGH": "AGENTDOCK_TEST_HOST_PASSTHROUGH",
	}

	command := `printf '%s' "$AGENTDOCK_TEST_CHILD_PASSTHROUGH"`
	if runtime.GOOS == "windows" {
		command = `[Console]::Out.Write($env:AGENTDOCK_TEST_CHILD_PASSTHROUGH)`
	}
	result, err := rt.execArgs(context.Background(), map[string]any{
		"cmd":            command,
		"yield_time_ms":  15000,
		"timeout_ms":     15000,
		"execution_mode": "sync",
	})
	if err != nil {
		t.Fatalf("exec_command should forward configured host env: %v", err)
	}
	if result["status"] != "exited" || result["stdout"].(string) != "host-forwarded" {
		t.Fatalf("configured host env was not forwarded: %#v", result)
	}
}

func TestExecCommandForwardsExplicitEnv(t *testing.T) {
	rt, _ := newCommandTestService(t)
	command := `printf '%s' "$AGENTDOCK_TEST_EXEC_ENV"`
	if runtime.GOOS == "windows" {
		command = `[Console]::Out.Write($env:AGENTDOCK_TEST_EXEC_ENV)`
	}
	result, err := rt.execArgs(context.Background(), map[string]any{
		"cmd":            command,
		"env":            map[string]any{"AGENTDOCK_TEST_EXEC_ENV": "forwarded"},
		"yield_time_ms":  15000,
		"timeout_ms":     15000,
		"execution_mode": "sync",
	})
	if err != nil {
		t.Fatalf("exec_command should accept explicit env: %v", err)
	}
	if result["status"] != "exited" || result["stdout"].(string) != "forwarded" {
		t.Fatalf("explicit env was not forwarded: %#v", result)
	}
}

func TestCommandEnvReportsTempDirectoryFailure(t *testing.T) {
	rt, cfg := newCommandTestService(t)
	root := cfg.AgentDockDefaultDir
	blocked := filepath.Join(root, "blocked-home")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.AgentDockHome = blocked
	if _, err := rt.CommandEnv("", nil); err == nil || !strings.Contains(err.Error(), "create command temp directory") {
		t.Fatalf("commandEnv() error = %v, want temp-directory error", err)
	}
}

func TestExecCommandForwardsStdinAndClosesPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test command uses POSIX cat")
	}
	rt, _ := newCommandTestService(t)
	result, err := rt.execArgs(context.Background(), map[string]any{
		"cmd":            "cat",
		"stdin":          "input-line\n",
		"yield_time_ms":  5000,
		"timeout_ms":     5000,
		"execution_mode": "sync",
	})
	if err != nil {
		t.Fatalf("execCommand() error = %v", err)
	}
	if result["status"] != "exited" || result["stdout"] != "input-line\n" {
		t.Fatalf("result = %#v", result)
	}
}

func TestExecCommandReportsClosedStdin(t *testing.T) {
	rt, _ := newCommandTestService(t)
	largeInput := strings.Repeat("x", 8<<20)
	_, err := rt.execArgs(context.Background(), map[string]any{
		"cmd":            stdinProbeCommand(t),
		"stdin":          largeInput,
		"env":            map[string]any{"GO_WANT_COMMAND_STDIN_PROBE": "1", "GO_WANT_COMMAND_STDIN_EARLY_EXIT": "1"},
		"yield_time_ms":  5000,
		"timeout_ms":     5000,
		"execution_mode": "sync",
	})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "STDIN_DELIVERY_FAILED" {
		t.Fatalf("execCommand() error = %#v, want STDIN_DELIVERY_FAILED", err)
	}
}

func TestExecCommandLargeStdinRoundTrip(t *testing.T) {
	rt, _ := newCommandTestService(t)
	for _, size := range []int{1 << 10, 64 << 10, 256 << 10, 1 << 20, 5 << 20} {
		t.Run(fmt.Sprintf("%d-bytes", size), func(t *testing.T) {
			input := strings.Repeat("中\\\"x", size/len([]byte("中\\\"x"))+1)
			data := []byte(input)
			if len(data) > size {
				data = data[:size]
				for !utf8.Valid(data) {
					data = data[:len(data)-1]
				}
			}
			input = string(data)
			sum := sha256.Sum256(data)
			result, err := rt.execArgs(context.Background(), map[string]any{
				"cmd":            stdinProbeCommand(t),
				"stdin":          input,
				"env":            map[string]any{"GO_WANT_COMMAND_STDIN_PROBE": "1"},
				"yield_time_ms":  15000,
				"timeout_ms":     30000,
				"execution_mode": "sync",
			})
			if err != nil {
				t.Fatalf("execCommand() error = %v", err)
			}
			want := fmt.Sprintf("%d %s", len(data), hex.EncodeToString(sum[:]))
			if result["status"] != "exited" || result["stdout"] != want {
				t.Fatalf("result = %#v, want stdout %q", result, want)
			}
			if result["stdin_expected_bytes"] != len(data) ||
				result["stdin_written_bytes"] != len(data) ||
				result["stdin_completed"] != true {
				t.Fatalf("stdin diagnostics = %#v", result)
			}
		})
	}
}
