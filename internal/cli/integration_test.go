package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aliyundrive-cli/internal/drive"
)

func TestCommandIntegration(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.URL.Path)
		if req.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing API authorization")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if req.URL.Path != "/v2/user/get" && body["drive_id"] != "selected-drive" {
			t.Errorf("drive_id = %v", body["drive_id"])
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v2/user/get":
			fmt.Fprint(w, `{"user_id":"user","nick_name":"test","default_drive_id":"default-drive"}`)
		case "/v2/file/get":
			fmt.Fprint(w, `{"file_id":"file","name":"report.pdf","parent_file_id":"root","type":"file","size":5}`)
		case "/adrive/v3/file/list":
			if body["marker"] == "page2" {
				fmt.Fprint(w, `{"items":[{"file_id":"two","name":"second.pdf","type":"file","size":2}],"next_marker":""}`)
			} else {
				fmt.Fprint(w, `{"items":[{"file_id":"one","name":"first.pdf","type":"file","size":1}],"next_marker":"page2"}`)
			}
		case "/v2/recyclebin/trash":
			fmt.Fprint(w, `{"async_task_id":"task"}`)
		default:
			t.Errorf("unexpected API endpoint %s", req.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	r := runner{newClient: func(dir string, timeout time.Duration) (*drive.Client, error) {
		if dir != "/explicit/config" {
			t.Errorf("config-dir = %q", dir)
		}
		if timeout != time.Minute {
			t.Errorf("timeout = %s", timeout)
		}
		return &drive.Client{HTTP: server.Client(), APIBase: server.URL, ConfigDir: dir, Token: drive.Token{AccessToken: "test-token"}}, nil
	}}
	run := func(args ...string) envelope {
		t.Helper()
		args = append(args, "--config-dir", "/explicit/config", "--drive-id", "selected-drive", "--timeout", "1m")
		var out, errOut bytes.Buffer
		if code := r.run(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("exit %d, output %s", code, out.String())
		}
		return decodeEnvelope(t, &out)
	}
	got := run("ls", "/", "--all")
	data := got.Data.(map[string]any)
	if len(data["items"].([]any)) != 2 || data["next_marker"] != "" {
		t.Fatalf("data = %#v", data)
	}
	if len(calls) != 2 {
		t.Fatalf("pagination calls = %#v", calls)
	}
	calls = nil
	got = run("trash", "id:file", "--dry-run")
	if got.Data.(map[string]any)["dry_run"] != true {
		t.Fatal("dry-run missing")
	}
	if len(calls) != 1 || calls[0] != "/v2/file/get" {
		t.Fatalf("dry-run performed writes: %v", calls)
	}
	calls = nil
	got = run("trash", "id:file", "--yes")
	if got.Data.(map[string]any)["async_task_id"] != "task" {
		t.Fatal("async task ID missing")
	}
	if len(calls) != 2 || calls[1] != "/v2/recyclebin/trash" {
		t.Fatalf("trash calls = %v", calls)
	}
	calls = nil
	got = run("auth", "status", "--check")
	if got.Data.(map[string]any)["authenticated"] != true {
		t.Fatal("online status was not authenticated")
	}
}

func TestNoCredentialsFailsWithoutNetwork(t *testing.T) {
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "")
	r := runner{newClient: func(dir string, timeout time.Duration) (*drive.Client, error) {
		return drive.NewClient(t.TempDir(), timeout)
	}}
	var out, errOut bytes.Buffer
	code := r.run(context.Background(), []string{"ls"}, strings.NewReader(""), &out, &errOut)
	got := decodeEnvelope(t, &out)
	if code != 3 || got.Error == nil || got.Error.Code != "AUTH_REQUIRED" {
		t.Fatalf("exit %d, response %#v", code, got)
	}
}

func TestRecoveryCommandsIgnoreCorruptCredentials(t *testing.T) {
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "invalid token value")
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		exit int
		code string
	}{
		{[]string{"auth", "import", "--stdin"}, 2, "INVALID_ARGUMENT"},
		{[]string{"auth", "poll"}, 3, "AUTH_SESSION_REQUIRED"},
		{[]string{"auth", "logout"}, 0, ""},
	} {
		var out, errOut bytes.Buffer
		args := append(tc.args, "--config-dir", dir)
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut)
		got := decodeEnvelope(t, &out)
		if code != tc.exit {
			t.Fatalf("%v: exit %d, error %#v", tc.args, code, got.Error)
		}
		if tc.code != "" && (got.Error == nil || got.Error.Code != tc.code) {
			t.Fatalf("%v: error %#v", tc.args, got.Error)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("logout did not remove corrupt credentials: %v", err)
	}
}

func TestPartialMkdirEffectsAreReturned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/adrive/v3/file/list":
			fmt.Fprint(w, `{"items":[],"next_marker":""}`)
		case "/adrive/v2/file/createWithFolders":
			if body["name"] == "a" {
				fmt.Fprint(w, `{"file_id":"created-a"}`)
			} else {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"code":"InternalError"}`)
			}
		default:
			t.Errorf("unexpected API path %s", req.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	r := runner{newClient: func(string, time.Duration) (*drive.Client, error) {
		return &drive.Client{HTTP: server.Client(), APIBase: server.URL, Token: drive.Token{AccessToken: "test", DriveID: "drive"}}, nil
	}}
	var out, errOut bytes.Buffer
	code := r.run(context.Background(), []string{"mkdir", "/a/b", "--parents"}, strings.NewReader(""), &out, &errOut)
	got := decodeEnvelope(t, &out)
	if code != 6 || got.OK || got.Error == nil || got.Error.Details == nil {
		t.Fatalf("exit %d, output %#v", code, got)
	}
	details := got.Error.Details.(map[string]any)
	created := details["created"].([]any)
	if len(created) != 1 || created[0].(map[string]any)["file_id"] != "created-a" {
		t.Fatalf("created = %#v", created)
	}
}
