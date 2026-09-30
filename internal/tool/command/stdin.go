package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type stdinMode string

const (
	stdinModeAuto stdinMode = "auto"
	stdinModePipe stdinMode = "pipe"
	stdinModeFile stdinMode = "file"

	stdinAutoSpoolThreshold = 64 << 10
	stdinSpoolRetention     = 24 * time.Hour
)

type stdinPreparation struct {
	configured bool
	requested  stdinMode
	used       stdinMode
	data       []byte
	expected   int
	written    int
	completed  bool
	spoolFile  *os.File
	spoolPath  string
}

func prepareInitialStdin(agentDockHome string, request ExecRequest) (*stdinPreparation, error) {
	rawMode := strings.ToLower(strings.TrimSpace(request.StdinMode))
	requested, used, err := resolveStdinMode(rawMode, request.TTY, len([]byte(request.Stdin)))
	if err != nil {
		return nil, err
	}
	if request.Stdin == "" {
		return &stdinPreparation{requested: requested, used: used}, nil
	}
	if !utf8.ValidString(request.Stdin) {
		return nil, toolError("STDIN_ENCODE_FAILED", "stdin must be valid UTF-8", "validation")
	}
	data := []byte(request.Stdin)
	prep := &stdinPreparation{
		configured: true,
		requested:  requested,
		used:       used,
		data:       data,
		expected:   len(data),
	}
	if used != stdinModeFile {
		return prep, nil
	}
	file, path, written, err := createStdinSpool(agentDockHome, data)
	if err != nil {
		return nil, err
	}
	prep.spoolFile = file
	prep.spoolPath = path
	prep.written = written
	prep.completed = written == len(data)
	return prep, nil
}

func resolveStdinMode(raw string, tty bool, size int) (stdinMode, stdinMode, error) {
	if raw == "" {
		raw = string(stdinModeAuto)
	}
	requested := stdinMode(raw)
	switch requested {
	case stdinModeAuto, stdinModePipe, stdinModeFile:
	default:
		return "", "", toolErrorDetails(
			"STDIN_MODE_INVALID",
			"stdin_mode must be auto, pipe, or file",
			"validation",
			map[string]any{"stdin_mode": raw, "allowed": []string{"auto", "pipe", "file"}},
		)
	}
	if tty && requested == stdinModeFile {
		return "", "", toolError(
			"STDIN_MODE_UNSUPPORTED_WITH_TTY",
			"stdin_mode=file is not supported with tty=true",
			"validation",
		)
	}
	used := requested
	if requested == stdinModeAuto {
		used = stdinModePipe
		if !tty && size >= stdinAutoSpoolThreshold {
			used = stdinModeFile
		}
	}
	return requested, used, nil
}

func createStdinSpool(agentDockHome string, data []byte) (*os.File, string, int, error) {
	dir := filepath.Join(agentDockHome, "tmp", "command-stdin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", 0, toolErrorDetails(
			"STDIN_SPOOL_CREATE_FAILED",
			"failed to create stdin spool directory",
			"runtime",
			map[string]any{"reason": err.Error()},
		)
	}
	scavengeStdinSpool(dir, stdinSpoolRetention)
	file, err := os.CreateTemp(dir, "stdin-*.bin")
	if err != nil {
		return nil, "", 0, toolErrorDetails(
			"STDIN_SPOOL_CREATE_FAILED",
			"failed to create stdin spool file",
			"runtime",
			map[string]any{"reason": err.Error()},
		)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, "", 0, toolErrorDetails(
			"STDIN_SPOOL_CREATE_FAILED",
			"failed to secure stdin spool file",
			"runtime",
			map[string]any{"reason": err.Error()},
		)
	}
	written, writeErr := writeAll(file, data)
	if writeErr != nil || written != len(data) {
		_ = file.Close()
		_ = os.Remove(path)
		reason := "short write"
		if writeErr != nil {
			reason = writeErr.Error()
		}
		return nil, "", written, toolErrorDetails(
			"STDIN_SPOOL_WRITE_FAILED",
			"failed to write complete stdin spool file",
			"runtime",
			map[string]any{"expected_bytes": len(data), "written_bytes": written, "reason": reason},
		)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, "", written, toolErrorDetails(
			"STDIN_SPOOL_WRITE_FAILED",
			"failed to flush stdin spool file",
			"runtime",
			map[string]any{"expected_bytes": len(data), "written_bytes": written, "reason": err.Error()},
		)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, "", written, toolErrorDetails(
			"STDIN_SPOOL_WRITE_FAILED",
			"failed to rewind stdin spool file",
			"runtime",
			map[string]any{"reason": err.Error()},
		)
	}
	return file, path, written, nil
}

func writeAll(writer io.Writer, data []byte) (int, error) {
	total := 0
	for total < len(data) {
		n, err := writer.Write(data[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (p *stdinPreparation) apply(command *exec.Cmd) {
	if p == nil || p.used != stdinModeFile || p.spoolFile == nil {
		return
	}
	command.Stdin = p.spoolFile
}

func (p *stdinPreparation) cleanupNow() {
	if p == nil {
		return
	}
	if p.spoolFile != nil {
		_ = p.spoolFile.Close()
		p.spoolFile = nil
	}
	if p.spoolPath != "" {
		_ = os.Remove(p.spoolPath)
		p.spoolPath = ""
	}
}

func (p *stdinPreparation) cleanupAfterStart(done <-chan struct{}) {
	if p == nil || p.spoolFile == nil {
		return
	}
	_ = p.spoolFile.Close()
	p.spoolFile = nil
	path := p.spoolPath
	if path == "" {
		return
	}
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		p.spoolPath = ""
		return
	}
	go func() {
		<-done
		_ = os.Remove(path)
	}()
	p.spoolPath = ""
}

func scavengeStdinSpool(dir string, retention time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "stdin-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

func stdinDiagnosticDetails(prep *stdinPreparation, written int, code string, err error) map[string]any {
	details := map[string]any{}
	if prep != nil {
		details["stdin_mode_requested"] = string(prep.requested)
		details["stdin_mode_used"] = string(prep.used)
		details["stdin_expected_bytes"] = prep.expected
		details["stdin_written_bytes"] = written
	}
	if code != "" {
		details["stdin_error_code"] = code
	}
	if err != nil {
		details["reason"] = fmt.Sprint(err)
	}
	return details
}
