package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func authTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := NewClient(filepath.Join(t.TempDir(), "config"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = server.Client()
	c.HTTP.Timeout = time.Second
	c.APIBase, c.AuthBase, c.PassportBase = server.URL, server.URL, server.URL
	return c
}

func requireDriveError(t *testing.T, err error, code string, exit int) *Error {
	t.Helper()
	var driveErr *Error
	if !errors.As(err, &driveErr) || driveErr.Code != code || driveErr.ExitCode != exit {
		t.Fatalf("error = %#v, want %s (exit %d)", err, code, exit)
	}
	return driveErr
}

func writeAuthJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func TestRequestNoCredentialsDoesNotContactNetwork(t *testing.T) {
	var requests atomic.Int32
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	err := c.Request(context.Background(), "/v2/user/get", struct{}{}, &User{})
	requireDriveError(t, err, "AUTH_REQUIRED", 3)
	if requests.Load() != 0 {
		t.Fatalf("made %d requests without credentials", requests.Load())
	}
	if _, err := os.Stat(c.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing credentials created config directory: %v", err)
	}
}

func TestRequestRefreshesOnceAfter401AndPersists(t *testing.T) {
	var calls, refreshes atomic.Int32
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token/refresh":
			refreshes.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != "old-refresh" || r.Header.Get("Authorization") != "" {
				t.Errorf("unexpected refresh request body or authorization")
			}
			writeAuthJSON(w, map[string]any{"access_token": "new-access", "refresh_token": "rotated-refresh", "expires_in": 7200, "default_drive_id": "drive-1"})
		case "/v2/user/get":
			calls.Add(1)
			if r.Header.Get("Authorization") != "Bearer new-access" {
				w.WriteHeader(http.StatusUnauthorized)
				writeAuthJSON(w, map[string]string{"code": "AccessTokenInvalid", "message": "old-access secret"})
				return
			}
			writeAuthJSON(w, User{UserID: "user-1", DefaultDriveID: "drive-1"})
		}
	})
	c.Token = Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiredAt: time.Now().Add(time.Hour)}
	user, err := c.User(context.Background())
	if err != nil || user.UserID != "user-1" {
		t.Fatalf("user = %#v, error = %v", user, err)
	}
	if refreshes.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("refreshes=%d requests=%d", refreshes.Load(), calls.Load())
	}
	loaded, err := NewClient(c.ConfigDir, time.Second)
	if err != nil || loaded.Token.RefreshToken != "rotated-refresh" || loaded.Token.DriveID != "drive-1" {
		t.Fatalf("reloaded token mismatch: client=%#v, error=%v", loaded, err)
	}
	assertPrivateAuthFile(t, filepath.Join(c.ConfigDir, "token.json"))
	info, _ := os.Stat(c.ConfigDir)
	if info.Mode().Perm() != 0700 {
		t.Errorf("config permissions=%o", info.Mode().Perm())
	}
}

func TestRequestDoesNotLoopRefreshOrRetryWrites(t *testing.T) {
	for _, status := range []int{401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var refreshes, writes atomic.Int32
			c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token/refresh" {
					refreshes.Add(1)
					writeAuthJSON(w, map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600})
					return
				}
				writes.Add(1)
				w.WriteHeader(status)
				writeAuthJSON(w, map[string]string{"code": "some-error"})
			})
			c.Token = Token{AccessToken: "access", RefreshToken: "refresh"}
			err := c.Request(context.Background(), "/v3/file/move", struct{}{}, nil)
			if status == 401 {
				requireDriveError(t, err, "AUTH_REQUIRED", 3)
				if refreshes.Load() != 1 || writes.Load() != 2 {
					t.Fatalf("refreshes=%d writes=%d", refreshes.Load(), writes.Load())
				}
			} else {
				if !requireDriveError(t, err, "API_ERROR", 6).Retryable {
					t.Error("server error should be retryable")
				}
				if refreshes.Load() != 0 || writes.Load() != 1 {
					t.Fatalf("refreshes=%d writes=%d", refreshes.Load(), writes.Load())
				}
			}
		})
	}
}

func TestAPIErrorClassificationAndSanitization(t *testing.T) {
	for _, test := range []struct {
		status int
		remote string
		code   string
		exit   int
		retry  bool
	}{
		{404, "", "NOT_FOUND", 4, false},
		{400, "FileIdNotFound", "NOT_FOUND", 4, false},
		{409, "", "CONFLICT", 5, false},
		{400, "AlreadyExist.File", "CONFLICT", 5, false},
		{403, "", "PERMISSION_DENIED", 6, false},
		{400, "InvalidRefreshToken", "AUTH_REQUIRED", 3, false},
		{429, "", "API_ERROR", 6, true},
		{503, "", "API_ERROR", 6, true},
		{200, "Forbidden.Account", "API_ERROR", 6, false},
	} {
		t.Run(fmt.Sprintf("%d/%s", test.status, test.remote), func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"code": test.remote, "message": "secret https://signed-url?token=secret"})
			err := requireDriveError(t, apiError(test.status, body), test.code, test.exit)
			if err.Retryable != test.retry || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https://") {
				t.Errorf("unsafe or incorrect error: %#v", err)
			}
		})
	}
}

func TestRequestContextCancellationAndTimeout(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	})
	c.Token.AccessToken = "access"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireDriveError(t, c.Request(ctx, "/test", struct{}{}, nil), "CANCELED", 8)
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	requireDriveError(t, c.Request(ctx, "/test", struct{}{}, nil), "TIMEOUT", 8)
}

func TestRequestDoesNotLeakCredentialsAcrossRedirects(t *testing.T) {
	var leaks atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaks.Add(1)
	}))
	defer other.Close()
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/leak", http.StatusTemporaryRedirect)
	})
	c.Token.AccessToken = "access-secret"
	// Even when both origins are trusted for login, bearer/body redirects are not.
	c.AuthBase = other.URL
	err := c.Request(context.Background(), "/v3/file/move", map[string]string{"token": "body-secret"}, nil)
	requireDriveError(t, err, "API_ERROR", 6)
	if leaks.Load() != 0 {
		t.Error("followed a redirect with credentials")
	}
}

func TestEnsureDriveUsesExplicitDriveWithoutNetwork(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("explicit drive should not require network access")
	})
	c.DriveID = "explicit-drive"
	c.Token.AccessToken = "access"
	id, err := c.EnsureDrive(context.Background())
	if err != nil || id != "explicit-drive" {
		t.Fatalf("id=%s error=%v", id, err)
	}
	c.Token = Token{}
	_, err = c.EnsureDrive(context.Background())
	requireDriveError(t, err, "AUTH_REQUIRED", 3)
}

func TestProactiveRefreshDoesNotRefreshAgainAfterUnauthorized(t *testing.T) {
	var refreshes atomic.Int32
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token/refresh" {
			refreshes.Add(1)
			writeAuthJSON(w, map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	c.Token.RefreshToken = "refresh"
	requireDriveError(t, c.Request(context.Background(), "/test", struct{}{}, nil), "AUTH_REQUIRED", 3)
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d want 1", refreshes.Load())
	}
}

func TestHTTPClientTimeoutIsRetryableAPIError(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	})
	c.Token.AccessToken = "access"
	c.HTTP.Timeout = 10 * time.Millisecond
	err := requireDriveError(t, c.Request(context.Background(), "/test", struct{}{}, nil), "TIMEOUT", 6)
	if !err.Retryable {
		t.Error("HTTP client timeout must be retryable")
	}
}

func TestRequestAcceptsNoContentForOptionalMetadata(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c.Token.AccessToken = "test-access"
	var response struct {
		AsyncTaskID string `json:"async_task_id"`
	}
	if err := c.Request(context.Background(), "/v2/recyclebin/trash", struct{}{}, &response); err != nil {
		t.Fatalf("204 No Content must be a successful mutation: %v", err)
	}
}
