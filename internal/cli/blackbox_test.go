package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aliyundrive-cli/internal/drive"
)

// Exercise the public argument/JSON contract with real clients and fresh
// credential loading on every invocation; only the remote service is simulated.
func TestBlackBoxCommandLifecycle(t *testing.T) {
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "")
	mock := &blackboxDrive{
		t: t,
		files: map[string]drive.File{
			"seed":   {FileID: "seed", DriveID: "selected-drive", ParentFileID: "root", Name: "seed.txt", Type: "file", Size: 4},
			"second": {FileID: "second", DriveID: "selected-drive", ParentFileID: "root", Name: "second.txt", Type: "file", Size: 6},
		},
		contents: map[string][]byte{"seed": []byte("seed"), "second": []byte("second")},
		pending:  map[string]bool{}, trashed: map[string]bool{},
	}
	server := httptest.NewServer(mock)
	defer server.Close()
	mock.base = server.URL
	dir := filepath.Join(t.TempDir(), "credentials")
	clientFactories, authFactories := 0, 0
	factory := func(auth bool) func(string, time.Duration) (*drive.Client, error) {
		return func(config string, timeout time.Duration) (*drive.Client, error) {
			if config != dir || timeout != 5*time.Second {
				t.Errorf("global flags not forwarded: config=%q timeout=%s", config, timeout)
			}
			var client *drive.Client
			var err error
			if auth {
				authFactories++
				client, err = drive.NewAuthClient(config, timeout)
			} else {
				clientFactories++
				client, err = drive.NewClient(config, timeout)
			}
			if err == nil {
				client.APIBase, client.AuthBase, client.PassportBase = server.URL, server.URL, server.URL
				client.HTTP.Transport = server.Client().Transport
			}
			return client, err
		}
	}
	r := runner{newClient: factory(false), newAuthClient: factory(true)}
	seen := map[string]bool{}
	run := func(wantExit int, input string, args ...string) envelope {
		t.Helper()
		original := append([]string{}, args...)
		if len(args) != 1 || (args[0] != "--version" && args[0] != "-v") {
			args = append([]string{"--config-dir", dir}, args...)
			args = append(args, "--timeout", "5s", "--drive-id=selected-drive")
		}
		var stdout, stderr bytes.Buffer
		exit := r.run(context.Background(), args, strings.NewReader(input), &stdout, &stderr)
		var got envelope
		decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		if err := decoder.Decode(&got); err != nil {
			t.Fatalf("%v: malformed JSON: %v, output %q", original, err, stdout.String())
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			t.Fatalf("%v: output was not exactly one JSON envelope", original)
		}
		if exit != wantExit || got.OK != (wantExit == 0) || got.SchemaVersion != "1" || stderr.Len() != 0 {
			t.Fatalf("%v: exit=%d want=%d envelope=%#v stderr=%q", original, exit, wantExit, got, stderr.String())
		}
		if wantExit != 0 && (got.Error == nil || got.Error.Code == "" || got.Data != nil) {
			t.Fatalf("%v: invalid error envelope: %#v", original, got)
		}
		for _, secret := range []string{"import-refresh-secret", "import-access-secret", "qr-access-secret", "qr-refresh-secret", "qr-ck-secret", "signed-storage-secret"} {
			if strings.Contains(stdout.String(), secret) {
				t.Fatalf("%v: secret appeared in output", original)
			}
		}
		seen[got.Command] = true
		return got
	}
	data := func(got envelope) map[string]any {
		t.Helper()
		value, ok := got.Data.(map[string]any)
		if !ok {
			t.Fatalf("expected JSON object data: %#v", got)
		}
		return value
	}
	fileID := func(got envelope) string {
		t.Helper()
		file, ok := data(got)["file"].(map[string]any)
		if !ok || file["file_id"] == "" {
			t.Fatalf("expected real file ID: %#v", got)
		}
		return file["file_id"].(string)
	}
	dry := func(args ...string) envelope {
		t.Helper()
		before, _ := mock.counts()
		got := run(0, "", append(args, "--dry-run")...)
		after, _ := mock.counts()
		if before != after || data(got)["dry_run"] != true {
			t.Fatalf("dry-run mutated remote state: %v", args)
		}
		return got
	}

	run(0, "", "schema", "--pretty")
	run(0, "")
	run(0, "", "help", "auth")
	run(0, "", "version")
	run(0, "", "--version")
	for _, command := range commands {
		run(0, "", append(strings.Fields(command.Name), "--help")...)
	}
	if clientFactories != 0 || authFactories != 0 {
		t.Fatal("schema/help/version instantiated an authenticated client")
	}
	if data(run(0, "", "auth", "status"))["authenticated"] != false {
		t.Fatal("fresh configuration unexpectedly authenticated")
	}
	run(0, "import-refresh-secret\n", "auth", "import", "--stdin")
	if data(run(0, "", "auth", "status", "--check"))["authenticated"] != true {
		t.Fatal("import did not persist usable credentials")
	}
	blackboxPrivate(t, dir, 0700)
	blackboxPrivate(t, filepath.Join(dir, "token.json"), 0600)
	tokenFile := filepath.Join(t.TempDir(), "legacy-token.json")
	if err := os.WriteFile(tokenFile, []byte(`{"refresh_token":"import-refresh-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte("corrupt credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	run(0, "", "auth", "import", "--token-file", tokenFile)

	page := data(run(0, "", "ls", "/", "--limit", "1"))
	if len(page["items"].([]any)) != 1 || page["next_marker"] == "" {
		t.Fatal("ls did not expose pagination")
	}
	page2 := data(run(0, "", "ls", "root", "--marker", page["next_marker"].(string), "--limit=1"))
	if len(page2["items"].([]any)) != 1 || page2["next_marker"] != "" {
		t.Fatal("ls did not consume continuation marker")
	}
	if len(data(run(0, "", "ls", "--all"))["items"].([]any)) != 2 {
		t.Fatal("ls --all lost items")
	}
	if data(run(0, "", "stat", "id:seed"))["file_id"] != "seed" || data(run(0, "", "stat", "/seed.txt"))["file_id"] != "seed" {
		t.Fatal("stat ID and path did not agree")
	}
	if len(data(run(0, "", "search", "seed", "--all", "--limit=1"))["items"].([]any)) != 1 {
		t.Fatal("search did not find the seed file")
	}
	dry("mkdir", "/planned/nested", "--parents")
	dry("mkdir", "/work")
	workID := fileID(run(0, "", "mkdir", "/work"))
	run(5, "", "mkdir", "/work")
	run(0, "", "mkdir", "/deep/path", "-p")
	run(0, "", "mkdir", "/deep/path", "--parents")
	dry("rename", "id:seed", "renamed.txt")
	run(0, "", "rename", "id:seed", "renamed.txt")
	run(5, "", "rename", "id:second", "renamed.txt")
	dry("mv", "id:seed", "id:"+workID)
	run(0, "", "mv", "id:seed", "id:"+workID)
	if data(run(0, "", "stat", "/work/renamed.txt"))["file_id"] != "seed" {
		t.Fatal("rename/move did not update path resolution")
	}
	_, requests := mock.counts()
	run(5, "", "trash", "id:seed")
	if _, after := mock.counts(); requests != after {
		t.Fatal("unconfirmed trash accessed the remote service")
	}
	dry("trash", "id:seed")
	run(0, "", "trash", "id:seed", "--yes")
	run(4, "", "stat", "id:seed")

	payload := bytes.Repeat([]byte("CLI payload\x00\xff\n"), 64)
	inputFile := filepath.Join(t.TempDir(), "local.bin")
	if err := os.WriteFile(inputFile, payload, 0600); err != nil {
		t.Fatal(err)
	}
	dry("upload", inputFile, "id:"+workID, "--name", "uploaded.bin")
	uploadID := fileID(run(0, "", "upload", inputFile, "id:"+workID, "--name=uploaded.bin"))
	run(5, "", "upload", inputFile, "id:"+workID, "--name", "uploaded.bin")
	downloadDir := t.TempDir()
	dry("download", "id:"+uploadID, downloadDir)
	if entries, err := os.ReadDir(downloadDir); err != nil || len(entries) != 0 {
		t.Fatal("dry download wrote local files")
	}
	run(0, "", "download", "id:"+uploadID, downloadDir)
	outputFile := filepath.Join(downloadDir, "uploaded.bin")
	if got, err := os.ReadFile(outputFile); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("upload/download payload round trip failed: %v", err)
	}
	run(5, "", "download", "id:"+uploadID, outputFile)
	if err := os.WriteFile(outputFile, []byte("replace me"), 0600); err != nil {
		t.Fatal(err)
	}
	run(0, "", "download", "id:"+uploadID, outputFile, "--overwrite")
	if got, err := os.ReadFile(outputFile); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("explicit download overwrite did not publish the complete file")
	}
	run(2, "", "download", "id:"+workID, outputFile)
	run(2, "", "upload", downloadDir, "/")
	run(0, "", "auth", "logout")
	_, requests = mock.counts()
	run(3, "", "ls")
	if _, after := mock.counts(); requests != after {
		t.Fatal("unauthenticated ls tried the network")
	}

	if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte("corrupt credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	login := data(run(0, "", "auth", "login"))
	if login["status"] != "pending" {
		t.Fatal("auth login did not return a pending session")
	}
	blackboxPrivate(t, login["qr_code_png"].(string), 0600)
	blackboxPrivate(t, filepath.Join(dir, "login-session.json"), 0600)
	if data(run(0, "", "auth", "poll"))["status"] != "pending" {
		t.Fatal("auth poll did not preserve pending session")
	}
	if data(run(0, "", "auth", "poll", "--wait"))["status"] != "authenticated" {
		t.Fatal("auth poll did not complete authentication")
	}
	for _, artifact := range []string{"login-session.json", "qr-code.png"} {
		if _, err := os.Stat(filepath.Join(dir, artifact)); !os.IsNotExist(err) {
			t.Fatalf("completed QR login retained %s", artifact)
		}
	}
	run(0, "", "auth", "status", "--check")
	run(0, "", "auth", "logout")
	if data(run(0, "", "auth", "status"))["authenticated"] != false {
		t.Fatal("logout did not remove credentials")
	}
	run(3, "", "auth", "poll")
	for _, command := range commands {
		if !seen[command.Name] {
			t.Errorf("command not covered: %s", command.Name)
		}
	}
	if clientFactories == 0 || authFactories == 0 {
		t.Fatal("both ordinary and recovery clients must be exercised")
	}
}

func blackboxPrivate(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != mode {
		t.Fatalf("private artifact %q: mode must be %o, error %v", path, mode, err)
	}
}

type blackboxDrive struct {
	t         *testing.T
	mu        sync.Mutex
	base      string
	files     map[string]drive.File
	contents  map[string][]byte
	pending   map[string]bool
	trashed   map[string]bool
	nextID    int
	mutations int
	requests  int
	polls     int
}

func (s *blackboxDrive) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutations, s.requests
}

func (s *blackboxDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	fail := func(status int, code string) {
		w.WriteHeader(status)
		write(map[string]string{"code": code})
	}
	if strings.HasPrefix(r.URL.Path, "/storage/") {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			s.t.Error("API credentials were forwarded to signed storage")
		}
		id := strings.TrimPrefix(r.URL.Path, "/storage/")
		if r.Method == http.MethodPut && s.pending[id] {
			payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				s.t.Error(err)
			}
			s.contents[id] = payload
			s.mutations++
			w.WriteHeader(200)
		} else if r.Method == http.MethodGet && !s.trashed[id] {
			_, _ = w.Write(s.contents[id])
		} else {
			fail(404, "FileNotFound")
		}
		return
	}
	if r.URL.Path == "/v2/oauth/authorize" {
		http.SetCookie(w, &http.Cookie{Name: "qr-session", Value: "persisted-session", Path: "/"})
		write(map[string]any{})
		return
	}
	if r.URL.Path == "/newlogin/qrcode/generate.do" || r.URL.Path == "/newlogin/qrcode/query.do" {
		if cookie, err := r.Cookie("qr-session"); err != nil || cookie.Value != "persisted-session" {
			s.t.Error("QR session cookie did not survive CLI invocations")
		}
		if r.URL.Path == "/newlogin/qrcode/generate.do" {
			write(map[string]any{"content": map[string]any{"data": map[string]any{"t": 123, "ck": "qr-ck-secret", "codeContent": "https://login.example/qr"}}})
			return
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("ck") != "qr-ck-secret" || r.PostForm.Get("t") != "123" {
			s.t.Error("QR poll did not reuse saved session arguments")
		}
		s.polls++
		status, biz := "NEW", ""
		if s.polls > 1 {
			status = "CONFIRMED"
			biz = base64.StdEncoding.EncodeToString([]byte(`{"pds_login_result":{"accessToken":"qr-access-secret","refreshToken":"qr-refresh-secret","expiresIn":3600,"defaultDriveId":"default-drive"}}`))
		}
		write(map[string]any{"content": map[string]any{"data": map[string]any{"qrCodeStatus": status, "bizExt": biz}}})
		return
	}
	if r.URL.Path == "/callback" {
		write(map[string]any{})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Errorf("decode %s request: %v", r.URL.Path, err)
		fail(400, "InvalidRequest")
		return
	}
	switch r.URL.Path {
	case "/token/refresh":
		if body["refresh_token"] != "import-refresh-secret" {
			s.t.Error("auth import used an unexpected refresh token")
		}
		write(map[string]any{"access_token": "import-access-secret", "refresh_token": "import-refresh-secret", "expires_in": 3600, "default_drive_id": "default-drive"})
		return
	case "/v2/oauth/token_login":
		write(map[string]string{"goto": s.base + "/callback?code=callback-code"})
		return
	case "/token/get":
		write(map[string]any{"access_token": "qr-access-secret", "refresh_token": "qr-refresh-secret", "expires_in": 3600, "default_drive_id": "default-drive"})
		return
	}
	if auth := r.Header.Get("Authorization"); auth != "Bearer import-access-secret" && auth != "Bearer qr-access-secret" {
		s.t.Errorf("missing API authorization for %s", r.URL.Path)
		fail(401, "AccessTokenInvalid")
		return
	}
	if r.URL.Path == "/v2/user/get" {
		write(drive.User{UserID: "mock-user", NickName: "Mock User", DefaultDriveID: "default-drive"})
		return
	}
	if body["drive_id"] != "selected-drive" {
		s.t.Errorf("selected drive was not forwarded to %s: %#v", r.URL.Path, body)
	}
	id, _ := body["file_id"].(string)
	file, found := s.files[id]
	switch r.URL.Path {
	case "/v2/file/get":
		if !found {
			fail(404, "FileNotFound")
		} else {
			write(struct {
				drive.File
				Trashed bool `json:"trashed"`
			}{file, s.trashed[id]})
		}
	case "/adrive/v3/file/list", "/adrive/v3/file/search":
		var items []drive.File
		name := ""
		if r.URL.Path == "/adrive/v3/file/search" {
			query, _ := body["query"].(string)
			if err := json.Unmarshal([]byte(strings.TrimPrefix(query, "name match ")), &name); err != nil {
				s.t.Errorf("malformed search query: %v", err)
			}
		}
		for key, child := range s.files {
			if s.trashed[key] || s.pending[key] {
				continue
			}
			if r.URL.Path == "/adrive/v3/file/list" && child.ParentFileID != body["parent_file_id"] {
				continue
			}
			if name != "" && !strings.Contains(child.Name, name) {
				continue
			}
			items = append(items, child)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		start, _ := strconv.Atoi(fmt.Sprint(body["marker"]))
		limit := int(body["limit"].(float64))
		end := min(start+limit, len(items))
		marker := ""
		if end < len(items) {
			marker = strconv.Itoa(end)
		}
		write(drive.Page{Items: items[start:end], NextMarker: marker})
	case "/adrive/v2/file/createWithFolders", "/v2/file/create_with_proof":
		if body["check_name_mode"] != "refuse" {
			s.t.Error("creation must refuse conflicts")
		}
		for key, child := range s.files {
			if !s.trashed[key] && child.ParentFileID == body["parent_file_id"] && child.Name == body["name"] {
				fail(409, "AlreadyExist")
				return
			}
		}
		s.nextID++
		id = fmt.Sprintf("created-%d", s.nextID)
		file = drive.File{FileID: id, DriveID: "selected-drive", ParentFileID: body["parent_file_id"].(string), Name: body["name"].(string), Type: body["type"].(string)}
		s.mutations++
		if file.Type == "folder" {
			s.files[id] = file
			write(map[string]string{"file_id": id})
		} else {
			file.Size = int64(body["size"].(float64))
			s.files[id], s.pending[id] = file, true
			write(map[string]any{"file_id": id, "upload_id": "upload-" + id, "part_info_list": []any{map[string]any{"part_number": 1, "upload_url": s.base + "/storage/" + id + "?signature=signed-storage-secret"}}})
		}
	case "/v3/file/update":
		file.Name = body["name"].(string)
		s.files[id] = file
		s.mutations++
		write(map[string]any{})
	case "/v3/file/move":
		if body["to_drive_id"] != "selected-drive" || body["check_name_mode"] != "refuse" {
			s.t.Error("move did not preserve drive or conflict protection")
		}
		file.ParentFileID = body["to_parent_file_id"].(string)
		s.files[id] = file
		s.mutations++
		write(map[string]any{})
	case "/v2/recyclebin/trash":
		s.trashed[id] = true
		s.mutations++
		write(map[string]string{"async_task_id": "trash-task"})
	case "/v2/file/complete":
		if !s.pending[id] || int64(len(s.contents[id])) != file.Size {
			s.t.Error("upload completion did not follow a complete payload")
		}
		delete(s.pending, id)
		s.mutations++
		write(file)
	case "/v2/file/get_download_url":
		write(map[string]string{"url": s.base + "/storage/" + id + "?signature=signed-storage-secret"})
	default:
		s.t.Errorf("unexpected service endpoint: %s", r.URL.Path)
		fail(500, "UnexpectedEndpoint")
	}
}
