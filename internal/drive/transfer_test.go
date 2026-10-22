package drive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeTransferInput(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadPartsBounded(t *testing.T) {
	for _, size := range []int64{0, 1, uploadPartSize, uploadPartSize + 1, uploadPartSize * 10000, uploadPartSize*10000 + 1, math.MaxInt64} {
		partSize, parts := uploadParts(size)
		if partSize < uploadPartSize || len(parts) > 10000 {
			t.Fatalf("unbounded plan for %d: %d, %d", size, partSize, len(parts))
		}
		if size == 0 && len(parts) != 1 {
			t.Fatal("empty file should request one empty storage part")
		}
		if size > 0 && int64(len(parts)) != size/partSize+boolInt(size%partSize != 0) {
			t.Fatal("multipart plan does not cover file size")
		}
	}
}

func TestUploadRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, dry := range []bool{false, true} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("dry=%t/symlink=%t", dry, symlink), func(t *testing.T) {
				fifo := filepath.Join(t.TempDir(), "input.fifo")
				if err := syscall.Mkfifo(fifo, 0600); err != nil {
					t.Fatal(err)
				}
				input := fifo
				if symlink {
					input += ".link"
					if err := os.Symlink(fifo, input); err != nil {
						t.Fatal(err)
					}
				}
				client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
					t.Errorf("invalid FIFO upload reached the network: %s", r.URL.Path)
					w.WriteHeader(500)
				})
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := client.Upload(ctx, input, "/", "", dry)
					done <- err
				}()
				select {
				case err := <-done:
					expectExit(t, err, 2)
				case <-time.After(250 * time.Millisecond):
					// Release an old blocking implementation before reporting its failure.
					writer, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0600)
					if err != nil {
						t.Fatalf("cannot release blocked FIFO reader: %v", err)
					}
					defer writer.Close()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("FIFO upload did not unblock after writer release")
					}
					t.Fatal("FIFO upload blocked past its command deadline")
				}
			})
		}
	}
}

func TestUploadCanceledBeforeLocalAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Client{}).Upload(ctx, filepath.Join(t.TempDir(), "missing"), "/", "", true)
	expectExit(t, err, 8)
}

func TestUploadStreamsMultipartWithoutCredentials(t *testing.T) {
	content := bytes.Repeat([]byte("x"), int(uploadPartSize)+3)
	input := writeTransferInput(t, content)
	partsRead := map[string]int{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials leaked to storage upload")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		partsRead[r.URL.Path] = len(data)
		if r.Method != http.MethodPut || r.ContentLength != int64(len(data)) {
			t.Errorf("incorrect part method or content length")
		}
		if r.Header.Get("Content-Type") != "" {
			t.Error("unsigned Content-Type header would invalidate signed storage authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer storage.Close()
	complete := false
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{}})
		case "/v2/file/create_with_proof":
			if body["check_name_mode"] != "refuse" || int64(body["size"].(float64)) != int64(len(content)) || len(body["part_info_list"].([]any)) != 2 {
				t.Errorf("incorrect upload plan: %#v", body)
			}
			respondJSON(w, map[string]any{"file_id": "created", "upload_id": "upload", "part_info_list": []uploadPart{{PartNumber: 2, UploadURL: storage.URL + "/2?secret=hidden"}, {PartNumber: 1, UploadURL: storage.URL + "/1?secret=hidden"}}})
		case "/v2/file/complete":
			complete = true
			respondJSON(w, File{FileID: "created", ContentHash: "hash"})
		default:
			t.Errorf("unexpected upload endpoint %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	jar, _ := cookiejar.New(nil)
	storageURL, _ := url.Parse(storage.URL)
	jar.SetCookies(storageURL, []*http.Cookie{{Name: "session", Value: "secret"}})
	client.HTTP.Jar = jar
	result, err := client.Upload(context.Background(), input, "/", "remote.bin", false)
	if err != nil || !complete || result.File.FileID != "created" || result.Bytes != int64(len(content)) {
		t.Fatalf("upload = %#v, %v", result, err)
	}
	if partsRead["/1"] != int(uploadPartSize) || partsRead["/2"] != 3 {
		t.Fatalf("wrong streamed sections: %#v", partsRead)
	}
}

func TestUploadZeroLengthAndDryRun(t *testing.T) {
	input := writeTransferInput(t, nil)
	creates, completes := 0, 0
	storageCalls := 0
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageCalls++
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			t.Errorf("empty upload was not a zero-length, non-chunked request: size=%d length=%d encoding=%v err=%v", len(body), r.ContentLength, r.TransferEncoding, err)
		}
		if r.Header.Get("Content-Type") != "" || r.Header.Get("Authorization") != "" || r.Method != http.MethodPut {
			t.Error("empty upload has invalid storage headers or method")
		}
		w.WriteHeader(200)
	}))
	defer storage.Close()
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{}})
		case "/v2/file/create_with_proof":
			creates++
			if len(body["part_info_list"].([]any)) != 1 || body["size"] != float64(0) {
				t.Error("invalid zero-length plan")
			}
			respondJSON(w, map[string]any{"file_id": "empty", "upload_id": "upload", "part_info_list": []uploadPart{{PartNumber: 1, UploadURL: storage.URL}}})
		case "/v2/file/complete":
			completes++
			respondJSON(w, File{FileID: "empty"})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	})
	if _, err := client.Upload(context.Background(), input, "/", "", true); err != nil || creates != 0 {
		t.Fatalf("dry upload mutated: %v", err)
	}
	result, err := client.Upload(context.Background(), input, "/", "", false)
	if err != nil || result.Bytes != 0 || creates != 1 || completes != 1 || storageCalls != 1 {
		t.Fatalf("empty upload = %#v, %v", result, err)
	}
}

func TestUploadFailureCleanupAndSecretRedaction(t *testing.T) {
	input := writeTransferInput(t, []byte("data"))
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `<Error><Code>SignatureDoesNotMatch</Code><Message>do-not-leak-response</Message><StringToSign>do-not-leak-signature</StringToSign></Error>`)
	}))
	defer storage.Close()
	cleanup := 0
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{}})
		case "/v2/file/create_with_proof":
			respondJSON(w, map[string]any{"file_id": "new", "upload_id": "upload", "part_info_list": []uploadPart{{PartNumber: 1, UploadURL: storage.URL + "/?signed=do-not-leak-url"}}})
		case "/v2/recyclebin/trash":
			cleanup++
			if body["file_id"] != "new" {
				t.Error("cleanup used wrong file")
			}
			respondJSON(w, map[string]any{})
		default:
			t.Errorf("failed part triggered unexpected request %s", r.URL.Path)
		}
	})
	_, err := client.Upload(context.Background(), input, "/", "", false)
	expectExit(t, err, 6)
	if cleanup != 1 || strings.Contains(fmt.Sprint(err), "do-not-leak") || strings.Contains(fmt.Sprint(err), storage.URL) {
		t.Fatalf("cleanup/redaction failed: %d %v", cleanup, err)
	}
	if !strings.Contains(err.Error(), "OSS SignatureDoesNotMatch") || err.(*Error).Retryable {
		t.Fatalf("safe OSS signature diagnostic missing or incorrectly retryable: %#v", err)
	}
}

func TestStorageResponseErrorsExposeOnlyKnownCodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantCode  string
		retryable bool
	}{
		{"known-code", 403, `<Error><Code>AccessDenied</Code><Message>secret-token</Message></Error>`, "AccessDenied", false},
		{"unknown-code", 403, `<Error><Code>secret-token</Code><Message>raw-secret</Message></Error>`, "", false},
		{"upstream", 500, `<Error><Code>InternalError</Code></Error>`, "InternalError", true},
		{"rate-limit", 429, `secret-token-not-xml`, "", true},
		{"bounded-malformed", 403, `<Error><Message>` + strings.Repeat("secret-token", 500) + `</Message><Code>AccessDenied</Code></Error>`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
			err := storageResponseError(context.Background(), response)
			expectExit(t, err, 6)
			if strings.Contains(err.Error(), "secret") || (tc.wantCode != "" && !strings.Contains(err.Error(), "OSS "+tc.wantCode)) || (tc.wantCode == "" && strings.Contains(err.Error(), "OSS ")) {
				t.Fatalf("unsafe or missing storage diagnostic: %v", err)
			}
			if err.(*Error).Retryable != tc.retryable {
				t.Fatalf("retryable = %t, want %t", err.(*Error).Retryable, tc.retryable)
			}
		})
	}
}

func TestUploadDoesNotCleanupExistingOrRapidFiles(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			input := writeTransferInput(t, []byte("data"))
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				switch r.URL.Path {
				case "/adrive/v3/file/list":
					respondJSON(w, Page{Items: []File{}})
				case "/v2/file/create_with_proof":
					respondJSON(w, map[string]any{"file_id": "file", "upload_id": "upload", "exist": existing, "rapid_upload": true})
				default:
					t.Errorf("existing or rapid upload caused request %s", r.URL.Path)
				}
			})
			_, err := client.Upload(context.Background(), input, "/", "", false)
			if existing {
				expectExit(t, err, 5)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUploadAmbiguousCompletionNeverTrashes(t *testing.T) {
	for _, completeStatus := range []int{200, 500} {
		t.Run(fmt.Sprint(completeStatus), func(t *testing.T) {
			input := writeTransferInput(t, nil)
			completed := false
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				switch r.URL.Path {
				case "/adrive/v3/file/list":
					respondJSON(w, Page{Items: []File{}})
				case "/v2/file/create_with_proof":
					respondJSON(w, map[string]any{"file_id": "possibly-complete", "upload_id": "upload", "part_info_list": []uploadPart{}})
				case "/v2/file/complete":
					completed = true
					w.WriteHeader(completeStatus)
					respondJSON(w, map[string]any{})
				default:
					t.Errorf("ambiguous completion triggered destructive cleanup: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			_, err := client.Upload(context.Background(), input, "/", "", false)
			expectExit(t, err, 6)
			if !completed || !strings.Contains(err.(*Error).Hint, "id:possibly-complete") {
				t.Fatalf("completion uncertainty lacks actionable ID: %v", err)
			}
		})
	}
}

func TestUploadCancellationDoesNotStartCleanup(t *testing.T) {
	input := writeTransferInput(t, []byte("data"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.WriteHeader(500)
	}))
	defer storage.Close()
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{}})
		case "/v2/file/create_with_proof":
			respondJSON(w, map[string]any{"file_id": "incomplete", "upload_id": "upload", "part_info_list": []uploadPart{{PartNumber: 1, UploadURL: storage.URL}}})
		default:
			t.Errorf("canceled upload started a new mutation: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	_, err := client.Upload(ctx, input, "/", "", false)
	expectExit(t, err, 8)
	if !strings.Contains(err.(*Error).Hint, "id:incomplete") {
		t.Fatal("canceled upload did not report its incomplete file ID")
	}
}

func TestUploadInvalidPlanDoesNotTrashRoot(t *testing.T) {
	input := writeTransferInput(t, nil)
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{}})
		case "/v2/file/create_with_proof":
			respondJSON(w, map[string]any{"file_id": "root", "upload_id": "upload"})
		default:
			t.Errorf("invalid plan triggered request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	_, err := client.Upload(context.Background(), input, "/", "", false)
	expectExit(t, err, 6)
}

func TestDownloadAtomicAndNoCredentialLeak(t *testing.T) {
	content := []byte("download content")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials leaked to storage download")
		}
		if r.Header.Get("Referer") != storageReferer || r.Header.Get("User-Agent") != storageUserAgent || r.Header.Get("Accept-Language") == "" {
			t.Error("storage download lacks required public web headers")
		}
		_, _ = w.Write(content)
	}))
	defer storage.Close()
	signedCalls := 0
	client := downloadTestClient(t, "remote.bin", int64(len(content)), storage.URL, &signedCalls)
	dir := t.TempDir()
	result, err := client.Download(context.Background(), "id:file", dir, false, true)
	if err != nil || signedCalls != 0 {
		t.Fatalf("dry download = %#v, %v", result, err)
	}
	assertDownloadDir(t, dir, 0)
	result, err = client.Download(context.Background(), "id:file", dir, false, false)
	if err != nil || result.Bytes != int64(len(content)) || signedCalls != 1 {
		t.Fatalf("download = %#v, %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "remote.bin"))
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("download content %q, %v", data, err)
	}
	assertDownloadDir(t, dir, 1)
	_, err = client.Download(context.Background(), "id:file", dir, false, false)
	expectExit(t, err, 5)
	if signedCalls != 1 {
		t.Fatal("existing destination requested storage URL")
	}
	_, err = client.Download(context.Background(), "id:file", dir, true, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDownloadPublicRefererSurvivesStorageRedirect(t *testing.T) {
	content := "download"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != storageReferer || strings.Contains(r.Header.Get("Referer"), "signed") || r.Header.Get("User-Agent") != storageUserAgent {
			t.Error("storage redirect leaked a signed Referer or lost required public web headers")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("storage redirect leaked credentials")
		}
		_, _ = io.WriteString(w, content)
	}))
	defer target.Close()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/redirected?signature=second-signed-secret", http.StatusFound)
	}))
	defer storage.Close()
	calls := 0
	client := downloadTestClient(t, "file.bin", int64(len(content)), storage.URL, &calls)
	_, err := client.Download(context.Background(), "id:file", t.TempDir(), false, false)
	if err != nil {
		t.Fatal(err)
	}
}

func downloadTestClient(t *testing.T, name string, size int64, storage string, signedCalls *int) *Client {
	t.Helper()
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/v2/file/get":
			respondJSON(w, File{FileID: "file", ParentFileID: "root", Name: name, Type: "file", Size: size})
		case "/v2/file/get_download_url":
			*signedCalls++
			respondJSON(w, map[string]string{"url": storage + "/?signed=do-not-leak"})
		default:
			t.Errorf("unexpected download request %s", r.URL.Path)
		}
	})
	jar, _ := cookiejar.New(nil)
	parsed, _ := url.Parse(storage)
	jar.SetCookies(parsed, []*http.Cookie{{Name: "session", Value: "secret"}})
	client.HTTP.Jar = jar
	return client
}

func assertDownloadDir(t *testing.T, dir string, count int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != count {
		t.Fatalf("unexpected destination contents: %#v, %v", entries, err)
	}
}

func TestDownloadSizeMismatchCleansTemporaryFile(t *testing.T) {
	for _, body := range []string{"short", "far too much content"} {
		t.Run(body, func(t *testing.T) {
			storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer storage.Close()
			calls := 0
			client := downloadTestClient(t, "file.bin", 10, storage.URL, &calls)
			dir := t.TempDir()
			_, err := client.Download(context.Background(), "id:file", dir, false, false)
			expectExit(t, err, 6)
			assertDownloadDir(t, dir, 0)
		})
	}
}

func TestDownloadRefusesPublicationRace(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(dest, []byte("race winner"), 0600); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, "download")
	}))
	defer storage.Close()
	calls := 0
	client := downloadTestClient(t, "file.bin", 8, storage.URL, &calls)
	_, err := client.Download(context.Background(), "id:file", dest, false, false)
	expectExit(t, err, 5)
	data, _ := os.ReadFile(dest)
	if string(data) != "race winner" {
		t.Fatal("download overwrote a concurrently created destination")
	}
	assertDownloadDir(t, dir, 1)
}

func TestDownloadRejectsUnsafeNameAndFolders(t *testing.T) {
	calls := 0
	client := downloadTestClient(t, "../unsafe", 0, "http://localhost", &calls)
	_, err := client.Download(context.Background(), "id:file", t.TempDir(), false, true)
	expectExit(t, err, 2)
	_, err = client.Download(context.Background(), "/", t.TempDir(), false, true)
	expectExit(t, err, 2)
	if calls != 0 {
		t.Fatal("unsafe download requested a URL")
	}
	_, err = client.Upload(context.Background(), t.TempDir(), "/", "", false)
	expectExit(t, err, 2)
}
