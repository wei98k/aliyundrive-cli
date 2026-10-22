package drive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fileTestClient(t *testing.T, handler func(http.ResponseWriter, *http.Request, map[string]any)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Errorf("API request lacks expected authorization")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode API request: %v", err)
			w.WriteHeader(400)
			return
		}
		handler(w, r, body)
	}))
	t.Cleanup(server.Close)
	return &Client{HTTP: server.Client(), APIBase: server.URL, DriveID: "drive", ConfigDir: t.TempDir(), Token: Token{AccessToken: "test-access", ExpiredAt: time.Now().Add(time.Hour)}}
}

func respondJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func expectExit(t *testing.T, err error, exit int) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.ExitCode != exit {
		t.Fatalf("error = %v, want exit %d", err, exit)
	}
}

func TestResolvePaginatedExactAndAmbiguous(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "duplicate"}[duplicate], func(t *testing.T) {
			var markers []string
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				if r.URL.Path != "/adrive/v3/file/list" || body["parent_file_id"] != "root" {
					t.Errorf("unexpected request: %s %#v", r.URL.Path, body)
				}
				marker, _ := body["marker"].(string)
				markers = append(markers, marker)
				if marker == "" {
					name := "Target"
					if duplicate {
						name = "target"
					}
					respondJSON(w, Page{Items: []File{{FileID: "first", Name: name, Type: "file"}}, NextMarker: "second"})
				} else {
					respondJSON(w, Page{Items: []File{{FileID: "second", Name: "target", Type: "file"}}})
				}
			})
			file, err := client.Resolve(context.Background(), "/target")
			if duplicate {
				expectExit(t, err, 5)
			} else if err != nil || file.FileID != "second" || file.DriveID != "drive" || file.ParentFileID != "root" {
				t.Fatalf("resolved %#v, %v", file, err)
			}
			if !reflect.DeepEqual(markers, []string{"", "second"}) {
				t.Fatalf("markers = %#v", markers)
			}
		})
	}
}

func TestInvalidReferencesBeforeRequests(t *testing.T) {
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		t.Errorf("invalid argument caused request %s", r.URL.Path)
		w.WriteHeader(500)
	})
	for _, ref := range []string{"relative", "//double", "/a/../b", "/a/./b", "/a//b", "id:", "id:..", "/bad\x00name", "id:bad/id"} {
		_, err := client.Resolve(context.Background(), ref)
		expectExit(t, err, 2)
	}
	for _, ref := range []string{"/", "root", "id:root"} {
		_, err := client.Rename(context.Background(), ref, "name", false)
		expectExit(t, err, 2)
		_, err = client.Move(context.Background(), ref, "/", false)
		expectExit(t, err, 2)
		_, err = client.Trash(context.Background(), ref, false)
		expectExit(t, err, 2)
	}
	_, err := client.List(context.Background(), "/", 101, "", false)
	expectExit(t, err, 2)
	_, err = client.Search(context.Background(), "name", 10, "bad\nmarker", false)
	expectExit(t, err, 2)
}

func TestListMarkerCycleAndCancellation(t *testing.T) {
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		respondJSON(w, Page{Items: []File{}, NextMarker: "loop"})
	})
	_, err := client.List(context.Background(), "/", 10, "", true)
	expectExit(t, err, 6)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.List(ctx, "/", 10, "", true)
	expectExit(t, err, 8)
}

func TestSearchEscapesQueryAndPaginates(t *testing.T) {
	name := "a\" and type = \"folder\\tail"
	quoted, _ := json.Marshal(name)
	calls := 0
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		calls++
		if r.URL.Path != "/adrive/v3/file/search" || body["query"] != "name match "+string(quoted) {
			t.Errorf("unsafe query: %#v", body)
		}
		if calls == 1 {
			respondJSON(w, Page{Items: []File{}, NextMarker: "next"})
		} else {
			if body["marker"] != "next" {
				t.Errorf("missing search marker")
			}
			respondJSON(w, Page{Items: []File{{FileID: "hit", Name: "hit"}}})
		}
	})
	page, err := client.Search(context.Background(), name, 10, "", true)
	if err != nil || len(page.Items) != 1 || calls != 2 {
		t.Fatalf("search = %#v, %v, calls %d", page, err, calls)
	}
}

func TestMkdirParentsDryRunWritesNothing(t *testing.T) {
	calls := 0
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		calls++
		if r.URL.Path != "/adrive/v3/file/list" || body["parent_file_id"] != "root" {
			t.Errorf("dry-run performed non-read or invented-parent request: %s %#v", r.URL.Path, body)
		}
		respondJSON(w, Page{Items: []File{}})
	})
	result, err := client.Mkdir(context.Background(), "/missing/nested", true, true)
	if err != nil || len(result.Created) != 2 || calls != 1 || result.File.Name != "nested" {
		t.Fatalf("dry-run = %#v %v, calls %d", result, err, calls)
	}
	for _, file := range result.Created {
		if file.FileID != "" {
			t.Fatal("dry-run fabricated a file ID")
		}
	}
}

func TestMkdirCreatesRefusingConflicts(t *testing.T) {
	created := 0
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{}})
		case "/adrive/v2/file/createWithFolders":
			created++
			if body["check_name_mode"] != "refuse" || body["type"] != "folder" {
				t.Errorf("unsafe folder creation: %#v", body)
			}
			id := "folder1"
			if created == 2 {
				id = "folder2"
				if body["parent_file_id"] != "folder1" {
					t.Error("nested folder not created in newly returned ID")
				}
			}
			respondJSON(w, map[string]string{"file_id": id, "file_name": body["name"].(string)})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	})
	result, err := client.Mkdir(context.Background(), "/one/two", true, false)
	if err != nil || result.File.FileID != "folder2" || created != 2 {
		t.Fatalf("mkdir = %#v, %v", result, err)
	}
}

func TestMoveRejectsDescendantsAndRenameConflict(t *testing.T) {
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/v2/file/get":
			id := body["file_id"].(string)
			parent := "root"
			if id == "descendant" {
				parent = "source"
			}
			respondJSON(w, File{FileID: id, Name: "source", ParentFileID: parent, Type: "folder"})
		case "/adrive/v3/file/list":
			respondJSON(w, Page{Items: []File{{FileID: "occupied", Name: "taken", Type: "file"}}})
		default:
			t.Errorf("invalid move/rename performed mutation %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	_, err := client.Move(context.Background(), "id:source", "id:descendant", false)
	expectExit(t, err, 2)
	_, err = client.Rename(context.Background(), "id:source", "taken", false)
	expectExit(t, err, 5)
}

func TestTrashUsesRecycleBin(t *testing.T) {
	mutations := 0
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.URL.Path == "/v2/file/get" {
			respondJSON(w, File{FileID: "file", Name: "x", ParentFileID: "root", Type: "file"})
			return
		}
		mutations++
		if r.URL.Path != "/v2/recyclebin/trash" || strings.Contains(r.URL.Path, "delete") {
			t.Errorf("wrong delete endpoint: %s", r.URL.Path)
		}
		respondJSON(w, map[string]string{"async_task_id": "task"})
	})
	_, err := client.Trash(context.Background(), "id:file", true)
	if err != nil || mutations != 0 {
		t.Fatalf("dry trash mutated: %v", err)
	}
	result, err := client.Trash(context.Background(), "id:file", false)
	if err != nil || mutations != 1 || result.AsyncTaskID != "task" {
		t.Fatalf("trash = %#v, %v", result, err)
	}
}
