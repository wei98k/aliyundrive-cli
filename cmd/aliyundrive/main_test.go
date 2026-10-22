package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type executableEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	OK            bool            `json:"ok"`
	Command       string          `json:"command"`
	Data          json.RawMessage `json:"data"`
	Error         *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func isolatedExecutableEnv(t *testing.T) []string {
	t.Helper()
	var env []string
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		switch strings.ToUpper(key) {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "ALIYUNDRIVE_REFRESH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		env = append(env, assignment)
	}
	home := t.TempDir()
	return append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"ALIYUNDRIVE_REFRESH_TOKEN=",
		// Fail closed if a regression unexpectedly attempts an HTTP request.
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"ALL_PROXY=",
		"NO_PROXY=",
	)
}

func newExecutableCommand(t *testing.T, binary string, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = isolatedExecutableEnv(t)
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	t.Cleanup(func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return cmd, stdout, stderr
}

func assertExecutableResult(t *testing.T, runErr error, stdout, stderr *bytes.Buffer, wantExit int, wantCommand, wantError string) executableEnvelope {
	t.Helper()
	exit := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("cannot execute CLI: %v; stdout=%q stderr=%q", runErr, stdout.String(), stderr.String())
		}
		exit = exitErr.ExitCode()
	}
	if exit != wantExit {
		t.Fatalf("exit=%d, want %d; error=%v stdout=%q stderr=%q", exit, wantExit, runErr, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var result executableEnvelope
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v; output=%q", err, stdout.String())
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout contains extra output: %v; output=%q", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.Command != wantCommand || result.OK != (wantExit == 0) {
		t.Fatalf("unexpected envelope: %+v", result)
	}
	if wantError == "" {
		if result.Error != nil || len(result.Data) == 0 {
			t.Fatalf("successful envelope has no data or has an error: %+v", result)
		}
	} else if result.Error == nil || result.Error.Code != wantError || len(result.Data) != 0 {
		t.Fatalf("unexpected error envelope: %+v", result)
	}
	return result
}

func TestExecutableOfflineContract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "aliyundrive")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}

	for _, tc := range []struct {
		name    string
		args    []string
		exit    int
		command string
		code    string
	}{
		{"version", []string{"version"}, 0, "version", ""},
		{"schema", []string{"schema"}, 0, "schema", ""},
		{"help", []string{"help", "upload"}, 0, "help", ""},
		{"invalid_arguments", []string{"stat"}, 2, "stat", "INVALID_ARGUMENT"},
		{"confirmation_required", []string{"trash", "id:offline-file"}, 5, "trash", "CONFIRMATION_REQUIRED"},
		{"authentication_required", []string{"ls"}, 3, "ls", "AUTH_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, stdout, stderr := newExecutableCommand(t, binary, tc.args...)
			assertExecutableResult(t, cmd.Run(), stdout, stderr, tc.exit, tc.command, tc.code)
		})
	}

	t.Run("stdin_deadline", func(t *testing.T) {
		cmd, stdout, stderr := newExecutableCommand(t, binary, "auth", "import", "--stdin", "--timeout", "30ms")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		// Keep stdin open without EOF so only the CLI's own deadline can finish the read.
		assertExecutableResult(t, cmd.Run(), stdout, stderr, 8, "auth import", "TIMEOUT")
	})

	t.Run("stdin_sigterm", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("SIGTERM is a POSIX signal")
		}
		cmd, stdout, stderr := newExecutableCommand(t, binary, "auth", "import", "--stdin", "--timeout", "1m")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		// Allow the executable to install its signal handler and block on stdin.
		select {
		case err := <-done:
			t.Fatalf("CLI exited before SIGTERM: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		case <-time.After(200 * time.Millisecond):
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		assertExecutableResult(t, <-done, stdout, stderr, 8, "auth import", "CANCELED")
	})
}
