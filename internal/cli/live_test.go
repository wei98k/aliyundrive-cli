package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aliyundrive-cli/internal/drive"
)

// TestLiveWorkflow is opt-in and only mutates its own randomly named directory.
// Credentials and downloaded fixtures stay in temporary directories, never logs.
func TestLiveWorkflow(t *testing.T) {
	config := os.Getenv("ALIYUNDRIVE_LIVE_TEST_CONFIG")
	if config == "" || os.Getenv("ALIYUNDRIVE_LIVE_ALLOW_WRITE") != "1" {
		t.Skip("requires ALIYUNDRIVE_LIVE_TEST_CONFIG and ALIYUNDRIVE_LIVE_ALLOW_WRITE=1")
	}
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "")
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "aliyundrive-cli-test-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(random[:])
	remote := "/" + name
	local := t.TempDir()
	payload := []byte("Aliyun Drive CLI live test: spaces, Unicode, and binary.\n\x00\x01\xff\n")
	source := filepath.Join(local, "source.bin")
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	emptySource := filepath.Join(local, "empty.bin")
	if err := os.WriteFile(emptySource, nil, 0600); err != nil {
		t.Fatal(err)
	}

	call := func(t *testing.T, want int, input io.Reader, args ...string) envelope {
		t.Helper()
		var output, stderr bytes.Buffer
		args = append(args, "--config-dir", config, "--timeout", "2m")
		code := Run(context.Background(), args, input, &output, &stderr)
		got := decodeEnvelope(t, &output)
		if code != want || got.OK != (want == 0) {
			t.Fatalf("%s: exit=%d expected=%d error=%#v", got.Command, code, want, got.Error)
		}
		if stderr.Len() != 0 {
			t.Fatal("unexpected stderr")
		}
		return got
	}
	data := func(got envelope) map[string]any { return got.Data.(map[string]any) }
	fileFrom := func(t *testing.T, got envelope) drive.File {
		t.Helper()
		encoded, err := json.Marshal(got.Data)
		if err != nil {
			t.Fatal(err)
		}
		var f drive.File
		if err := json.Unmarshal(encoded, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	transferFile := func(t *testing.T, got envelope) drive.File {
		t.Helper()
		encoded, _ := json.Marshal(data(got)["file"])
		var f drive.File
		if err := json.Unmarshal(encoded, &f); err != nil {
			t.Fatal(err)
		}
		if f.FileID == "" || f.FileID == "root" {
			t.Fatal("transfer returned no valid ID")
		}
		return f
	}

	if !t.Run("AuthStatusAndTokenFileImport", func(t *testing.T) {
		if data(call(t, 0, nil, "auth", "status"))["authenticated"] != true {
			t.Fatal("no local authentication")
		}
		if data(call(t, 0, nil, "auth", "status", "--check"))["authenticated"] != true {
			t.Fatal("online validation failed")
		}
		call(t, 0, nil, "auth", "import", "--token-file", filepath.Join(config, "token.json"))
	}) {
		return
	}
	if !t.Run("AuthStdinImport", func(t *testing.T) {
		credentials, err := os.ReadFile(filepath.Join(config, "token.json"))
		if err != nil {
			t.Fatal("cannot read temporary test credentials")
		}
		call(t, 0, bytes.NewReader(credentials), "auth", "import", "--stdin")
	}) {
		return
	}
	if !t.Run("MkdirDryRunNoWrite", func(t *testing.T) {
		call(t, 0, nil, "mkdir", remote+"/source/nested", "--parents", "--dry-run")
		call(t, 4, nil, "stat", remote)
	}) {
		return
	}
	var root drive.File
	t.Cleanup(func() {
		if root.FileID == "" {
			var out, errOut bytes.Buffer
			code := Run(context.Background(), []string{"stat", remote, "--config-dir", config, "--timeout", "30s"}, nil, &out, &errOut)
			if code == 4 {
				return
			}
			if code != 0 {
				t.Errorf("cannot locate partial test directory %s for cleanup", remote)
				return
			}
			root = fileFrom(t, decodeEnvelope(t, &out))
		}
		if root.Name != name || root.FileID == "" || root.FileID == "root" {
			t.Error("refusing cleanup of unverified test directory")
			return
		}
		var out, errOut bytes.Buffer
		code := Run(context.Background(), []string{"trash", "id:" + root.FileID, "--yes", "--config-dir", config, "--timeout", "30s"}, nil, &out, &errOut)
		if code != 0 {
			t.Errorf("cleanup failed; inspect test directory id:%s", root.FileID)
		} else {
			t.Log("isolated test directory moved to recycle bin")
		}
	})
	if !t.Run("MkdirParentsAndStat", func(t *testing.T) {
		call(t, 0, nil, "mkdir", remote+"/source/nested", "--parents")
		root = fileFrom(t, call(t, 0, nil, "stat", remote))
		if root.Name != name || root.FileID == "" || root.FileID == "root" {
			t.Fatal("invalid isolated test directory")
		}
		call(t, 0, nil, "mkdir", remote+"/destination")
		call(t, 0, nil, "mkdir", remote+"/source/nested", "--parents")
		call(t, 5, nil, "mkdir", remote+"/source/nested")
	}) {
		return
	}
	t.Run("ListPaginationAndAll", func(t *testing.T) {
		page := data(call(t, 0, nil, "ls", "id:"+root.FileID, "--limit", "1"))
		if len(page["items"].([]any)) != 1 || page["next_marker"] == "" {
			t.Fatal("expected a continuation marker")
		}
		call(t, 0, nil, "ls", "id:"+root.FileID, "--limit", "1", "--marker", page["next_marker"].(string))
		all := data(call(t, 0, nil, "ls", remote, "--all", "--limit", "1"))
		if len(all["items"].([]any)) != 2 || all["next_marker"] != "" {
			t.Fatal("incomplete --all result")
		}
	})
	t.Run("Search", func(t *testing.T) {
		for attempt := 0; attempt < 15; attempt++ {
			result := data(call(t, 0, nil, "search", name, "--all"))
			for _, raw := range result["items"].([]any) {
				if raw.(map[string]any)["file_id"] == root.FileID {
					return
				}
			}
			time.Sleep(time.Second)
		}
		t.Fatal("new test directory was not found by search")
	})
	remoteName := "round trip-\u6d4b\u8bd5.bin"
	var uploaded drive.File
	if !t.Run("UploadDryRunAndNamedUpload", func(t *testing.T) {
		call(t, 0, nil, "upload", source, remote+"/source", "--name", remoteName, "--dry-run")
		call(t, 4, nil, "stat", remote+"/source/"+remoteName)
		uploaded = transferFile(t, call(t, 0, nil, "upload", source, remote+"/source", "--name", remoteName))
		if uploaded.Name != remoteName || uploaded.Size != int64(len(payload)) {
			t.Fatal("upload metadata mismatch")
		}
		call(t, 5, nil, "upload", source, remote+"/source", "--name", remoteName)
	}) {
		return
	}
	t.Run("DownloadDryRunRoundTripAndOverwrite", func(t *testing.T) {
		dest := filepath.Join(local, "downloaded.bin")
		call(t, 0, nil, "download", "id:"+uploaded.FileID, dest, "--dry-run")
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("dry-run created a local file")
		}
		call(t, 0, nil, "download", "id:"+uploaded.FileID, dest)
		got, err := os.ReadFile(dest)
		if err != nil || sha256.Sum256(got) != sha256.Sum256(payload) {
			t.Fatal("download SHA-256 mismatch")
		}
		call(t, 5, nil, "download", "id:"+uploaded.FileID, dest)
		call(t, 0, nil, "download", "id:"+uploaded.FileID, dest, "--overwrite")
	})
	t.Run("RenameDryRunMutationAndNoOp", func(t *testing.T) {
		call(t, 0, nil, "rename", "id:"+uploaded.FileID, "renamed.bin", "--dry-run")
		if fileFrom(t, call(t, 0, nil, "stat", "id:"+uploaded.FileID)).Name != remoteName {
			t.Fatal("dry-run renamed file")
		}
		call(t, 0, nil, "rename", "id:"+uploaded.FileID, "renamed.bin")
		if fileFrom(t, call(t, 0, nil, "stat", "id:"+uploaded.FileID)).Name != "renamed.bin" {
			t.Fatal("rename not applied")
		}
		call(t, 0, nil, "rename", "id:"+uploaded.FileID, "renamed.bin")
	})
	t.Run("MoveDryRunMutationAndNoOp", func(t *testing.T) {
		folder := fileFrom(t, call(t, 0, nil, "stat", remote+"/destination"))
		call(t, 0, nil, "mv", "id:"+uploaded.FileID, "id:"+folder.FileID, "--dry-run")
		if fileFrom(t, call(t, 0, nil, "stat", "id:"+uploaded.FileID)).ParentFileID != uploaded.ParentFileID {
			t.Fatal("dry-run moved file")
		}
		call(t, 0, nil, "mv", "id:"+uploaded.FileID, "id:"+folder.FileID)
		if fileFrom(t, call(t, 0, nil, "stat", "id:"+uploaded.FileID)).ParentFileID != folder.FileID {
			t.Fatal("move not applied")
		}
		call(t, 0, nil, "mv", "id:"+uploaded.FileID, "id:"+folder.FileID)
	})
	t.Run("EmptyFileRoundTrip", func(t *testing.T) {
		f := transferFile(t, call(t, 0, nil, "upload", emptySource, remote+"/source"))
		dest := filepath.Join(local, "empty-download.bin")
		call(t, 0, nil, "download", "id:"+f.FileID, dest)
		info, err := os.Stat(dest)
		if err != nil || info.Size() != 0 {
			t.Fatal("empty file round trip failed")
		}
	})
	t.Run("MultipartRoundTrip", func(t *testing.T) {
		content := bytes.Repeat([]byte("0123456789abcdef"), (11*1024*1024)/16)
		largeSource := filepath.Join(local, "multipart.bin")
		if err := os.WriteFile(largeSource, content, 0600); err != nil {
			t.Fatal(err)
		}
		f := transferFile(t, call(t, 0, nil, "upload", largeSource, remote+"/source"))
		dest := filepath.Join(local, "multipart-download.bin")
		call(t, 0, nil, "download", "id:"+f.FileID, dest)
		got, err := os.ReadFile(dest)
		if err != nil || sha256.Sum256(got) != sha256.Sum256(content) {
			t.Fatal("multipart SHA-256 mismatch")
		}
	})
	t.Run("TrashGuardDryRunAndConfirmation", func(t *testing.T) {
		call(t, 5, nil, "trash", "id:"+uploaded.FileID)
		call(t, 0, nil, "trash", "id:"+uploaded.FileID, "--dry-run")
		call(t, 0, nil, "stat", "id:"+uploaded.FileID)
		call(t, 0, nil, "trash", "id:"+uploaded.FileID, "--yes")
		call(t, 4, nil, "stat", "id:"+uploaded.FileID)
	})
	// Logout is tested in an isolated copy, so the rest of this account session is intact.
	t.Run("LogoutIsolatedCredentials", func(t *testing.T) {
		copyDir := t.TempDir()
		credentialData, err := os.ReadFile(filepath.Join(config, "token.json"))
		if err != nil {
			t.Fatal("cannot read temporary test credentials")
		}
		if err := os.Chmod(copyDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copyDir, "token.json"), credentialData, 0600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := Run(context.Background(), []string{"auth", "logout", "--config-dir", copyDir}, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("logout exit %d", code)
		}
		if _, err := os.Stat(filepath.Join(copyDir, "token.json")); !os.IsNotExist(err) {
			t.Fatal("logout did not remove credentials")
		}
	})
	t.Log(fmt.Sprintf("test directory: id:%s; all uploads were test fixtures", root.FileID))
}
