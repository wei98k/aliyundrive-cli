package drive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestImportTokenAcceptsRawAndSDKJSON(t *testing.T) {
	for _, input := range []string{"import-refresh\n", `{"access_token":"ignored-old-access","refresh_token":"import-refresh","expired_at":"2020-01-01T00:00:00Z"}`} {
		t.Run(input[:5], func(t *testing.T) {
			c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token/refresh":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["refresh_token"] != "import-refresh" {
						t.Error("incorrect imported refresh token")
					}
					writeAuthJSON(w, map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh", "expires_in": 3600})
				case "/v2/user/get":
					writeAuthJSON(w, User{UserID: "import-user", DefaultDriveID: "import-drive"})
				}
			})
			user, err := c.ImportToken(context.Background(), strings.NewReader(input))
			if err != nil || user.UserID != "import-user" || c.Token.RefreshToken != "fresh-refresh" || c.Token.DriveID != "import-drive" {
				t.Fatalf("user=%#v token=%#v error=%v", user, c.Token, err)
			}
			assertPrivateAuthFile(t, filepath.Join(c.ConfigDir, "token.json"))
		})
	}
}

func TestImportTokenUnclosedPipeRespectsDeadline(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("incomplete input must not contact the API")
	})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := c.ImportToken(ctx, reader)
		result <- err
	}()
	select {
	case err := <-result:
		requireDriveError(t, err, "TIMEOUT", 8)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("token import ignored its deadline while waiting for input EOF")
	}
	if _, err := os.Stat(c.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled import changed credentials: %v", err)
	}
}

type authTrackedReader struct {
	io.Reader
	closed atomic.Bool
}

func (r *authTrackedReader) Close() error {
	r.closed.Store(true)
	return nil
}

func TestReadTokenInputPreservesSuccessfulReaderAndSanitizesErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader := &authTrackedReader{Reader: strings.NewReader("test-refresh")}
	data, err := readTokenInput(ctx, reader)
	if err != nil || string(data) != "test-refresh" || reader.closed.Load() {
		t.Fatalf("successful read incorrectly closed input or failed: %v", err)
	}
	_, err = readTokenInput(ctx, authErrorReader{})
	apiErr := requireDriveError(t, err, "IO_ERROR", 7)
	if strings.Contains(apiErr.Error(), "test-token-secret") {
		t.Error("input error leaked reader content")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, err = readTokenInput(canceled, reader)
	requireDriveError(t, err, "CANCELED", 8)
	if reader.closed.Load() {
		t.Error("pre-canceled operation should not access or close input")
	}
}

func TestReadTokenInputInheritedBlockingPipeRespectsDeadline(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	// Fd puts this file in the same non-pollable blocking mode inherited stdin
	// can use. Close need not interrupt the syscall until the writer reaches EOF.
	_ = reader.Fd()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := readTokenInput(ctx, reader)
		done <- err
	}()
	select {
	case err := <-done:
		requireDriveError(t, err, "TIMEOUT", 8)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("inherited pipe ignored input deadline")
	}
}

type authBlockingCloser struct {
	readRelease  chan struct{}
	closeRelease chan struct{}
}

func (r *authBlockingCloser) Read([]byte) (int, error) {
	<-r.readRelease
	return 0, io.EOF
}

func (r *authBlockingCloser) Close() error {
	<-r.closeRelease
	close(r.readRelease)
	return nil
}

func TestReadTokenInputBlockingCloseCannotBlockDeadline(t *testing.T) {
	reader := &authBlockingCloser{readRelease: make(chan struct{}), closeRelease: make(chan struct{})}
	defer close(reader.closeRelease)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := readTokenInput(ctx, reader)
		done <- err
	}()
	select {
	case err := <-done:
		requireDriveError(t, err, "TIMEOUT", 8)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("blocking custom Close defeated input deadline")
	}
}

type authErrorReader struct{}

func (authErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("test-token-secret")
}

func TestFailedTokenImportPreservesSavedAccount(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeAuthJSON(w, map[string]string{"code": "InvalidRefreshToken", "message": "new-token-secret"})
	})
	c.Token = Token{RefreshToken: "saved-account-refresh", AccessToken: "saved-account-access"}
	if err := c.saveToken(); err != nil {
		t.Fatal(err)
	}
	_, err := c.ImportToken(context.Background(), strings.NewReader("new-token-secret"))
	requireDriveError(t, err, "AUTH_REQUIRED", 3)
	var saved Token
	if err := readPrivateJSON(filepath.Join(c.ConfigDir, "token.json"), &saved); err != nil || saved.RefreshToken != "saved-account-refresh" || c.Token.RefreshToken != saved.RefreshToken {
		t.Fatalf("existing account overwritten; error=%v", err)
	}
}

func TestRejectedImportUserDoesNotRefreshOrPersistNewAccount(t *testing.T) {
	var refreshes atomic.Int32
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token/refresh" {
			refreshes.Add(1)
			writeAuthJSON(w, map[string]any{"access_token": "rejected-access", "refresh_token": "rotated-rejected-refresh", "expires_in": 1})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	c.Token = Token{RefreshToken: "saved-refresh", AccessToken: "saved-access"}
	if err := c.saveToken(); err != nil {
		t.Fatal(err)
	}
	_, err := c.ImportToken(context.Background(), strings.NewReader("imported-refresh"))
	requireDriveError(t, err, "AUTH_REQUIRED", 3)
	var saved Token
	if err := readPrivateJSON(filepath.Join(c.ConfigDir, "token.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 || c.Token.RefreshToken != "saved-refresh" || saved.RefreshToken != "saved-refresh" {
		t.Fatalf("failed validation changed account: refreshes=%d memory=%s disk=%s", refreshes.Load(), c.Token.RefreshToken, saved.RefreshToken)
	}
}

func TestQRLoginSurvivesSeparateProcessesAndCompletes(t *testing.T) {
	var polls atomic.Int32
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/oauth/authorize":
			http.SetCookie(w, &http.Cookie{Name: "qr-session", Value: "session-secret", Path: "/", HttpOnly: true})
			w.WriteHeader(http.StatusOK)
		case "/newlogin/qrcode/generate.do":
			if cookie, err := r.Cookie("qr-session"); err != nil || cookie.Value != "session-secret" {
				t.Error("QR generation missing login cookie")
			}
			writeAuthJSON(w, map[string]any{"content": map[string]any{"data": map[string]any{"t": 12345, "ck": "qr-secret", "codeContent": "https://example.invalid/login/qr"}}})
		case "/newlogin/qrcode/query.do":
			if cookie, err := r.Cookie("qr-session"); err != nil || cookie.Value != "session-secret" {
				t.Error("QR poll missing persisted login cookie")
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("t") != "12345" || r.Form.Get("ck") != "qr-secret" {
				t.Error("incorrect QR poll form")
			}
			status, encoded := "SCANED", ""
			if polls.Add(1) == 2 {
				status = "CONFIRMED"
				data, _ := json.Marshal(map[string]any{"pds_login_result": map[string]any{"accessToken": "qr-access", "refreshToken": "qr-refresh", "expiresIn": 3600, "defaultDriveId": "qr-drive"}})
				encoded = base64.StdEncoding.EncodeToString(data)
			}
			writeAuthJSON(w, map[string]any{"content": map[string]any{"data": map[string]string{"qrCodeStatus": status, "bizExt": encoded}}})
		case "/v2/oauth/token_login":
			writeAuthJSON(w, map[string]string{"goto": ""})
		case "/v2/user/get":
			if r.Header.Get("Authorization") != "Bearer qr-access" {
				t.Error("incorrect QR authorization")
			}
			writeAuthJSON(w, User{UserID: "qr-user", DefaultDriveID: "qr-drive"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	session, err := c.LoginStart(context.Background())
	if err != nil || session.Status != "pending" || session.QRCodeURL == "" {
		t.Fatalf("session=%#v error=%v", session, err)
	}
	assertPrivateAuthFile(t, session.QRCodePNG)
	assertPrivateAuthFile(t, c.sessionPath())
	for i := 0; i < 2; i++ {
		fresh, err := NewClient(c.ConfigDir, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		fresh.APIBase, fresh.AuthBase, fresh.PassportBase = c.APIBase, c.AuthBase, c.PassportBase
		result, err := fresh.LoginPoll(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && result.Status != "scanned" || i == 1 && (result.Status != "authenticated" || result.User == nil || result.User.UserID != "qr-user") {
			t.Fatalf("poll %d result=%#v", i, result)
		}
	}
	for _, path := range []string{c.sessionPath(), session.QRCodePNG} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("login session not removed after success")
		}
	}
	loaded, err := NewClient(c.ConfigDir, time.Second)
	if err != nil || loaded.Token.RefreshToken != "qr-refresh" {
		t.Fatalf("credential save error=%v", err)
	}
}

func TestQRPollTerminalStatesAndMissingSession(t *testing.T) {
	for _, state := range []string{"NEW", "EXPIRED", "CANCELED", "UNEXPECTED"} {
		t.Run(state, func(t *testing.T) {
			c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeAuthJSON(w, map[string]any{"content": map[string]any{"data": map[string]string{"qrCodeStatus": state}}})
			})
			_, err := c.LoginPoll(context.Background(), false)
			requireDriveError(t, err, "AUTH_SESSION_REQUIRED", 3)
			if err := writePrivateJSON(c.sessionPath(), qrSession{T: 1, CK: "secret", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			result, err := c.LoginPoll(context.Background(), false)
			if state == "UNEXPECTED" {
				requireDriveError(t, err, "AUTH_ERROR", 3)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"NEW": "pending", "EXPIRED": "expired", "CANCELED": "canceled"}[state]
			if result.Status != want {
				t.Fatalf("status=%s want %s", result.Status, want)
			}
		})
	}
}

func TestQRPollLocallyExpiredSessionMakesNoRequests(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("expired local session should not contact the service")
	})
	if err := writePrivateJSON(c.sessionPath(), qrSession{T: 1, CK: "old-ck", CreatedAt: time.Now().Add(-11 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(c.ConfigDir, "qr-code.png"), []byte("obsolete-qr")); err != nil {
		t.Fatal(err)
	}
	result, err := c.LoginPoll(context.Background(), false)
	if err != nil || result.Status != "expired" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	for _, path := range []string{c.sessionPath(), filepath.Join(c.ConfigDir, "qr-code.png")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Error("expired session retained private login artifacts")
		}
	}
}

func TestQRPollWaitRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeAuthJSON(w, map[string]any{"content": map[string]any{"data": map[string]string{"qrCodeStatus": "NEW"}}})
		cancel()
	})
	if err := writePrivateJSON(c.sessionPath(), qrSession{T: 1, CK: "secret", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err := c.LoginPoll(ctx, true)
	requireDriveError(t, err, "CANCELED", 8)
}

func qrTestEncodedCredentials(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"pds_login_result": map[string]any{
		"accessToken": "qr-access", "refreshToken": "qr-refresh", "expiresIn": 3600, "defaultDriveId": "qr-drive",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func TestQRPollWaitCompletesCallbackAndTokenExchange(t *testing.T) {
	encoded := qrTestEncodedCredentials(t)
	var base string
	var polls, callbacks, exchanges atomic.Int32
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/newlogin/qrcode/query.do":
			status := "NEW"
			if polls.Add(1) > 1 {
				status = "CONFIRMED"
			}
			writeAuthJSON(w, map[string]any{"content": map[string]any{"data": map[string]string{"qrCodeStatus": status, "bizExt": encoded}}})
		case "/v2/oauth/token_login":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["token"] != "qr-access" || r.Header.Get("Authorization") != "" {
				t.Error("invalid token login request")
			}
			writeAuthJSON(w, map[string]string{"goto": base + "/sign/callback?code=callback-code"})
		case "/sign/callback":
			callbacks.Add(1)
			if r.URL.Query().Get("code") != "callback-code" || r.Header.Get("Authorization") != "" {
				t.Error("invalid callback request")
			}
			http.SetCookie(w, &http.Cookie{Name: "oauth-callback", Value: "callback-session", Path: "/", HttpOnly: true})
			w.WriteHeader(http.StatusOK)
		case "/token/get":
			exchanges.Add(1)
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["code"] != "callback-code" || input["loginType"] != "normal" || input["deviceId"] != "aliyundrive" {
				t.Error("invalid OAuth code exchange")
			}
			if cookie, err := r.Cookie("oauth-callback"); err != nil || cookie.Value != "callback-session" {
				t.Error("token exchange lost callback cookie")
			}
			writeAuthJSON(w, map[string]any{"access_token": "exchanged-access", "refresh_token": "exchanged-refresh", "expires_in": 3600, "default_drive_id": "exchanged-drive"})
		case "/v2/user/get":
			if r.Header.Get("Authorization") != "Bearer exchanged-access" {
				t.Error("profile validation did not use exchanged credentials")
			}
			writeAuthJSON(w, User{UserID: "callback-user", DefaultDriveID: "exchanged-drive"})
		default:
			t.Errorf("unexpected QR request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	base = c.APIBase
	if err := writePrivateJSON(c.sessionPath(), qrSession{T: 1, CK: "test-ck", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := c.LoginPoll(ctx, true)
	if err != nil || result.Status != "authenticated" || result.User == nil || result.User.UserID != "callback-user" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if polls.Load() != 2 || callbacks.Load() != 1 || exchanges.Load() != 1 || c.Token.RefreshToken != "exchanged-refresh" || c.Token.DriveID != "exchanged-drive" {
		t.Fatalf("unexpected flow counts polls=%d callbacks=%d exchanges=%d", polls.Load(), callbacks.Load(), exchanges.Load())
	}
	var saved Token
	if err := readPrivateJSON(filepath.Join(c.ConfigDir, "token.json"), &saved); err != nil || saved.RefreshToken != "exchanged-refresh" {
		t.Fatalf("exchanged token was not persisted: %v", err)
	}
	if _, err := os.Stat(c.sessionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("completed wait retained login session")
	}
}

func TestQRLoginRejectsExternalRedirectAndPreservesAccount(t *testing.T) {
	encoded := qrTestEncodedCredentials(t)
	var externalRequests atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalRequests.Add(1)
	}))
	defer external.Close()
	for _, indirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "untrusted-goto", true: "untrusted-http-redirect"}[indirect], func(t *testing.T) {
			var base string
			c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/oauth/token_login" {
					gotoURL := external.URL + "/steal?code=private-oauth-code"
					if indirect {
						gotoURL = base + "/redirect?code=private-oauth-code"
					}
					writeAuthJSON(w, map[string]string{"goto": gotoURL})
					return
				}
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, external.URL+"/steal", http.StatusFound)
					return
				}
				t.Errorf("unexpected request %s", r.URL.Path)
			})
			base = c.APIBase
			c.Token = Token{AccessToken: "existing-access", RefreshToken: "existing-refresh"}
			if err := c.saveToken(); err != nil {
				t.Fatal(err)
			}
			_, err := c.finishLogin(context.Background(), encoded)
			if indirect {
				requireDriveError(t, err, "API_ERROR", 6)
			} else {
				requireDriveError(t, err, "AUTH_ERROR", 3)
			}
			if strings.Contains(err.Error(), "private-oauth-code") || strings.Contains(err.Error(), external.URL) {
				t.Error("redirect error leaked private authentication data")
			}
			var saved Token
			if err := readPrivateJSON(filepath.Join(c.ConfigDir, "token.json"), &saved); err != nil || saved.RefreshToken != "existing-refresh" || c.Token.RefreshToken != "existing-refresh" {
				t.Fatalf("failed QR login overwrote existing account: %v", err)
			}
		})
	}
	if externalRequests.Load() != 0 {
		t.Fatalf("followed %d untrusted callback redirects", externalRequests.Load())
	}
}

func TestQRLoginRejectedProfilePreservesSavedAccount(t *testing.T) {
	encoded := qrTestEncodedCredentials(t)
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/oauth/token_login" {
			writeAuthJSON(w, map[string]string{"goto": ""})
			return
		}
		if r.URL.Path != "/v2/user/get" {
			t.Errorf("QR profile failure unexpectedly triggered %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	c.Token = Token{AccessToken: "existing-access", RefreshToken: "existing-refresh"}
	if err := c.saveToken(); err != nil {
		t.Fatal(err)
	}
	_, err := c.finishLogin(context.Background(), encoded)
	requireDriveError(t, err, "AUTH_REQUIRED", 3)
	var saved Token
	if err := readPrivateJSON(filepath.Join(c.ConfigDir, "token.json"), &saved); err != nil || saved.RefreshToken != "existing-refresh" || c.Token.RefreshToken != "existing-refresh" {
		t.Fatalf("rejected QR profile changed saved account: %v", err)
	}
}

func TestStatusAndLogout(t *testing.T) {
	c := authTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeAuthJSON(w, User{UserID: "status-user", DefaultDriveID: "status-drive"})
	})
	status, err := c.Status(context.Background(), false)
	if err != nil || status.Authenticated {
		t.Fatalf("unexpected empty status=%#v error=%v", status, err)
	}
	c.Token = Token{AccessToken: "status-access", RefreshToken: "status-refresh", ExpiredAt: time.Now().Add(time.Hour)}
	if err := c.saveToken(); err != nil {
		t.Fatal(err)
	}
	status, err = c.Status(context.Background(), true)
	if err != nil || !status.Authenticated || status.User == nil || status.User.UserID != "status-user" {
		t.Fatalf("unexpected validated status=%#v error=%v", status, err)
	}
	if err := c.Logout(); err != nil {
		t.Fatal(err)
	}
	if c.Token.AccessToken != "" || c.Token.RefreshToken != "" {
		t.Error("logout retained credentials in memory")
	}
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "token.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("logout retained credentials on disk")
	}
	if err := c.Logout(); err != nil {
		t.Fatalf("logout must be idempotent: %v", err)
	}
}
