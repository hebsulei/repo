package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveStdinModeValidation(t *testing.T) {
	if _, _, err := resolveStdinMode("invalid", false, 1); err == nil {
		t.Fatal("resolveStdinMode(invalid) error = nil")
	} else {
		var toolErr *ToolError
		if !errors.As(err, &toolErr) || toolErr.Code != "STDIN_MODE_INVALID" {
			t.Fatalf("resolveStdinMode(invalid) error = %#v", err)
		}
	}

	if _, _, err := resolveStdinMode("file", true, 1); err == nil {
		t.Fatal("resolveStdinMode(file, tty=true) error = nil")
	} else {
		var toolErr *ToolError
		if !errors.As(err, &toolErr) || toolErr.Code != "STDIN_MODE_UNSUPPORTED_WITH_TTY" {
			t.Fatalf("resolveStdinMode(file, tty=true) error = %#v", err)
		}
	}
}

func TestResolveStdinModeAutoSelection(t *testing.T) {
	requested, used, err := resolveStdinMode("", false, stdinAutoSpoolThreshold-1)
	if err != nil {
		t.Fatal(err)
	}
	if requested != stdinModeAuto || used != stdinModePipe {
		t.Fatalf("small auto = %s/%s, want auto/pipe", requested, used)
	}

	requested, used, err = resolveStdinMode("auto", false, stdinAutoSpoolThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if requested != stdinModeAuto || used != stdinModeFile {
		t.Fatalf("large auto = %s/%s, want auto/file", requested, used)
	}

	requested, used, err = resolveStdinMode("auto", true, stdinAutoSpoolThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if requested != stdinModeAuto || used != stdinModePipe {
		t.Fatalf("tty auto = %s/%s, want auto/pipe", requested, used)
	}
}

func TestPrepareInitialStdinFileCleanup(t *testing.T) {
	home := t.TempDir()
	prep, err := prepareInitialStdin(home, ExecRequest{Stdin: "payload", StdinMode: "file"})
	if err != nil {
		t.Fatal(err)
	}
	path := prep.spoolPath
	if path == "" {
		t.Fatal("spool path is empty")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat spool: %v", err)
	} else if info.Size() != int64(len("payload")) {
		t.Fatalf("spool size = %d", info.Size())
	}
	prep.cleanupNow()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spool still exists after cleanup: %v", err)
	}
}

func TestExecCommandFileStdinCleansSpool(t *testing.T) {
	rt, cfg := newCommandTestService(t)
	input := string(make([]byte, 256<<10))
	result, err := rt.execArgs(context.Background(), map[string]any{
		"cmd":            stdinProbeCommand(t),
		"stdin":          input,
		"stdin_mode":     "file",
		"env":            map[string]any{"GO_WANT_COMMAND_STDIN_PROBE": "1"},
		"yield_time_ms":  5000,
		"timeout_ms":     10000,
		"execution_mode": "sync",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["stdin_mode_requested"] != "file" ||
		result["stdin_mode_used"] != "file" ||
		result["stdin_completed"] != true {
		t.Fatalf("stdin diagnostics = %#v", result)
	}

	dir := filepath.Join(cfg.AgentDockHome, "tmp", "command-stdin")
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, readErr := os.ReadDir(dir)
		if os.IsNotExist(readErr) {
			return
		}
		if readErr != nil {
			t.Fatalf("read spool dir: %v", readErr)
		}
		if len(entries) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spool files remain after command: %v", entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
