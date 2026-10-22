package drive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

type storedCookie struct {
	URL    string      `json:"url"`
	Cookie http.Cookie `json:"cookie"`
}

// The standard cookie jar is intentionally in-memory; this wrapper preserves the
// QR session across separate auth login and auth poll processes.
type sessionJar struct {
	jar     *cookiejar.Jar
	mu      sync.Mutex
	cookies map[string]storedCookie
}

func newSessionJar() *sessionJar {
	jar, _ := cookiejar.New(nil)
	return &sessionJar{jar: jar, cookies: make(map[string]storedCookie)}
}

func (j *sessionJar) Cookies(u *url.URL) []*http.Cookie { return j.jar.Cookies(u) }

func (j *sessionJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.jar.SetCookies(u, cookies)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cookie := range cookies {
		origin := u.Scheme + "://" + u.Host
		domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
		if domain == "" {
			domain = strings.ToLower(u.Hostname())
		}
		path := cookie.Path
		if !strings.HasPrefix(path, "/") {
			path = u.Path
			if index := strings.LastIndex(path, "/"); index > 0 {
				path = path[:index]
			} else {
				path = "/"
			}
		}
		key := origin + "\x00" + domain + "\x00" + path + "\x00" + cookie.Name
		if cookie.MaxAge < 0 || !cookie.Expires.IsZero() && cookie.Expires.Before(time.Now()) {
			delete(j.cookies, key)
		} else {
			j.cookies[key] = storedCookie{URL: origin + u.EscapedPath(), Cookie: *cookie}
		}
	}
}

func (j *sessionJar) snapshot() []storedCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	cookies := make([]storedCookie, 0, len(j.cookies))
	for _, cookie := range j.cookies {
		cookies = append(cookies, cookie)
	}
	return cookies
}

type qrSession struct {
	T         int64          `json:"t"`
	CK        string         `json:"ck"`
	CreatedAt time.Time      `json:"created_at"`
	Cookies   []storedCookie `json:"cookies"`
}

func (c *Client) ensureJar() {
	if c.jar == nil {
		c.jar = newSessionJar()
	}
	c.HTTP.Jar = c.jar
}

func (c *Client) sessionPath() string { return filepath.Join(c.ConfigDir, "login-session.json") }

func (c *Client) LoginStart(ctx context.Context) (LoginSession, error) {
	c.ensureJar()
	query := url.Values{
		"client_id": {"25dzX3vbYqktVxyX"}, "redirect_uri": {"https://www.aliyundrive.com/sign/callback"},
		"response_type": {"code"}, "login_type": {"custom"}, "state": {`{"origin":"https://www.aliyundrive.com"}`},
	}
	if err := c.do(ctx, http.MethodGet, strings.TrimRight(c.AuthBase, "/")+"/v2/oauth/authorize?"+query.Encode(), "", nil, "", nil); err != nil {
		return LoginSession{}, err
	}
	var response struct {
		Content struct {
			Data struct {
				TitleMsg    string `json:"title_msg"`
				T           int64  `json:"t"`
				CodeContent string `json:"codeContent"`
				CK          string `json:"ck"`
				ResultCode  int    `json:"resultCode"`
			} `json:"data"`
		} `json:"content"`
	}
	query = url.Values{
		"appName": {"aliyun_drive"}, "fromSite": {"52"}, "appEntrance": {"web"},
		"isMobile": {"false"}, "lang": {"zh_CN"}, "returnUrl": {""}, "bizParams": {""}, "_bx-v": {"2.0.31"},
	}
	if err := c.do(ctx, http.MethodGet, strings.TrimRight(c.PassportBase, "/")+"/newlogin/qrcode/generate.do?"+query.Encode(), "", nil, "", &response); err != nil {
		return LoginSession{}, err
	}
	data := response.Content.Data
	if data.TitleMsg != "" || data.T == 0 || data.CK == "" || data.CodeContent == "" || len(data.CodeContent) > 8192 {
		return LoginSession{}, E("AUTH_ERROR", "The service could not create a QR login session.", "Try aliyundrive auth login again.", 3)
	}
	png, err := qrcode.Encode(data.CodeContent, qrcode.Medium, 320)
	if err != nil {
		return LoginSession{}, E("AUTH_ERROR", "Cannot encode the login QR code.", "", 3)
	}
	pngPath := filepath.Join(c.ConfigDir, "qr-code.png")
	if err := writePrivate(pngPath, png); err != nil {
		return LoginSession{}, err
	}
	session := qrSession{T: data.T, CK: data.CK, CreatedAt: time.Now(), Cookies: c.jar.snapshot()}
	if err := writePrivateJSON(c.sessionPath(), session); err != nil {
		_ = os.Remove(pngPath)
		return LoginSession{}, err
	}
	return LoginSession{Status: "pending", QRCodeURL: data.CodeContent, QRCodePNG: pngPath}, nil
}

func (c *Client) allowedLoginURL(u *url.URL) bool {
	if u.Scheme != "https" && u.Scheme != "http" || u.User != nil {
		return false
	}
	for _, base := range []string{c.AuthBase, c.APIBase, c.PassportBase} {
		parsed, err := url.Parse(base)
		if err == nil && parsed.Scheme == u.Scheme && parsed.Host == u.Host {
			return true
		}
	}
	return u.Scheme == "https" && (u.Host == "www.aliyundrive.com" || u.Host == "aliyundrive.com")
}

func (c *Client) loadSession() (qrSession, error) {
	var session qrSession
	if err := readPrivateJSON(c.sessionPath(), &session); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return session, E("AUTH_SESSION_REQUIRED", "No pending QR login session exists.", "Run aliyundrive auth login first.", 3)
		}
		return session, err
	}
	if session.T == 0 || session.CK == "" || session.CreatedAt.IsZero() {
		return session, E("AUTH_SESSION_REQUIRED", "The saved QR login session is invalid.", "Run aliyundrive auth login again.", 3)
	}
	c.ensureJar()
	for _, cookie := range session.Cookies {
		u, err := url.Parse(cookie.URL)
		if err == nil && c.allowedLoginURL(u) {
			copy := cookie.Cookie
			c.jar.SetCookies(u, []*http.Cookie{&copy})
		}
	}
	return session, nil
}

type qrQueryResponse struct {
	Content struct {
		Data struct {
			Status string `json:"qrCodeStatus"`
			BizExt string `json:"bizExt"`
		} `json:"data"`
	} `json:"content"`
}

func (c *Client) LoginPoll(ctx context.Context, wait bool) (LoginResult, error) {
	session, err := c.loadSession()
	if err != nil {
		return LoginResult{}, err
	}
	if time.Since(session.CreatedAt) > 10*time.Minute {
		_ = c.clearSession()
		return LoginResult{Status: "expired"}, nil
	}
	for {
		form := url.Values{
			"t": {strconv.FormatInt(session.T, 10)}, "ck": {session.CK}, "appName": {"aliyun_drive"},
			"appEntrance": {"web"}, "isMobile": {"false"}, "lang": {"zh_CN"}, "returnUrl": {""},
			"fromSite": {"52"}, "bizParams": {""}, "navlanguage": {"zh-CN"}, "navPlatform": {"MacIntel"},
		}
		var response qrQueryResponse
		err := c.do(ctx, http.MethodPost, strings.TrimRight(c.PassportBase, "/")+"/newlogin/qrcode/query.do?appName=aliyun_drive&fromSite=52&_bx-v=2.0.31", "application/x-www-form-urlencoded", []byte(form.Encode()), "", &response)
		if err != nil {
			return LoginResult{}, err
		}
		status := ""
		switch strings.ToUpper(response.Content.Data.Status) {
		case "NEW":
			status = "pending"
		case "SCANED", "SCANNED":
			status = "scanned"
		case "EXPIRED":
			status = "expired"
		case "CANCELED", "CANCELLED":
			status = "canceled"
		case "CONFIRMED":
			return c.finishLogin(ctx, response.Content.Data.BizExt)
		default:
			return LoginResult{}, E("AUTH_ERROR", "The service returned an unknown QR login state.", "Run aliyundrive auth login again.", 3)
		}
		if status == "expired" || status == "canceled" {
			if err := c.clearSession(); err != nil {
				return LoginResult{}, err
			}
			return LoginResult{Status: status}, nil
		}
		session.Cookies = c.jar.snapshot()
		if err := writePrivateJSON(c.sessionPath(), session); err != nil {
			return LoginResult{}, err
		}
		if !wait {
			return LoginResult{Status: status}, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return LoginResult{}, networkError(ctx, ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) finishLogin(ctx context.Context, encoded string) (LoginResult, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	var payload struct {
		Token struct {
			AccessToken    string `json:"accessToken"`
			RefreshToken   string `json:"refreshToken"`
			ExpiresIn      int    `json:"expiresIn"`
			DefaultDriveID string `json:"defaultDriveId"`
		} `json:"pds_login_result"`
	}
	if err != nil || json.Unmarshal(data, &payload) != nil || !validToken(payload.Token.AccessToken) || !validToken(payload.Token.RefreshToken) {
		return LoginResult{}, E("AUTH_ERROR", "The service returned invalid QR login credentials.", "Run aliyundrive auth login again.", 3)
	}
	tokens := tokenResponse{AccessToken: payload.Token.AccessToken, RefreshToken: payload.Token.RefreshToken, ExpiresIn: payload.Token.ExpiresIn, DefaultDriveID: payload.Token.DefaultDriveID}
	var confirmation struct {
		Goto string `json:"goto"`
	}
	if err := c.jsonRequest(ctx, http.MethodPost, strings.TrimRight(c.AuthBase, "/")+"/v2/oauth/token_login", map[string]string{"token": payload.Token.AccessToken}, "", &confirmation); err != nil {
		return LoginResult{}, err
	}
	if confirmation.Goto != "" {
		u, err := url.Parse(confirmation.Goto)
		if err != nil || !c.allowedLoginURL(u) {
			return LoginResult{}, E("AUTH_ERROR", "The service returned an invalid login redirect.", "", 3)
		}
		if err := c.do(ctx, http.MethodGet, u.String(), "", nil, "", nil); err != nil {
			return LoginResult{}, err
		}
		if code := u.Query().Get("code"); code != "" {
			if err := c.jsonRequest(ctx, http.MethodPost, strings.TrimRight(c.APIBase, "/")+"/token/get", map[string]string{"code": code, "loginType": "normal", "deviceId": "aliyundrive"}, "", &tokens); err != nil {
				return LoginResult{}, err
			}
		}
	}
	token, err := tokens.token(payload.Token.RefreshToken)
	if err != nil {
		return LoginResult{}, err
	}
	previous := c.Token
	c.Token = token
	user, err := c.userWithToken(ctx)
	if err != nil {
		c.Token = previous
		return LoginResult{}, err
	}
	if c.Token.DriveID == "" {
		c.Token.DriveID = user.DefaultDriveID
	}
	if err := c.saveToken(); err != nil {
		c.Token = previous
		return LoginResult{}, err
	}
	if err := c.clearSession(); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Status: "authenticated", User: &user}, nil
}

func readTokenInput(ctx context.Context, reader io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, networkError(ctx, err)
	}
	type readResult struct {
		data []byte
		err  error
	}
	read := func() readResult {
		data, err := io.ReadAll(io.LimitReader(reader, 65537))
		return readResult{data: data, err: err}
	}
	var result readResult
	if ctx.Done() == nil {
		result = read()
	} else {
		finished := make(chan readResult, 1)
		go func() { finished <- read() }()
		select {
		case result = <-finished:
		case <-ctx.Done():
			// Close only on cancellation, never after a successful stdin read.
			// Closing normally releases pipes/files; an arbitrary Reader or a
			// non-pollable file can retain this one size-limited read until EOF.
			// A custom Close must not be allowed to block the deadline either.
			if closer, ok := reader.(io.ReadCloser); ok {
				go func() { _ = closer.Close() }()
			}
			return nil, networkError(ctx, ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, networkError(ctx, err)
	}
	if result.err != nil {
		return nil, E("IO_ERROR", "Cannot read the refresh token input.", "", 7)
	}
	return result.data, nil
}

func (c *Client) ImportToken(ctx context.Context, reader io.Reader) (User, error) {
	data, err := readTokenInput(ctx, reader)
	if err != nil {
		return User{}, err
	}
	if len(data) > 65536 {
		return User{}, Invalid("Token input exceeds the size limit")
	}
	input := strings.TrimSpace(string(data))
	var token Token
	if strings.HasPrefix(input, "{") {
		if err := json.Unmarshal(data, &token); err != nil {
			return User{}, Invalid("Token input must be a refresh token or token JSON")
		}
	} else {
		token.RefreshToken = input
	}
	if !validToken(token.RefreshToken) {
		return User{}, Invalid("Token input contains no valid refresh_token")
	}
	// Validate imported credentials before replacing an existing saved account.
	candidate := *c
	candidate.Token = Token{RefreshToken: token.RefreshToken}
	candidate.DriveID = ""
	if err := candidate.refresh(ctx, false); err != nil {
		return User{}, err
	}
	user, err := candidate.userWithToken(ctx)
	if err != nil {
		return User{}, err
	}
	if candidate.Token.DriveID == "" {
		candidate.Token.DriveID = user.DefaultDriveID
	}
	if err := candidate.saveToken(); err != nil {
		return User{}, err
	}
	c.Token = candidate.Token
	return user, nil
}

func (c *Client) clearSession() error {
	for _, name := range []string{"login-session.json", "qr-code.png"} {
		if err := os.Remove(filepath.Join(c.ConfigDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return configError("Cannot remove the saved login session.")
		}
	}
	return nil
}

func (c *Client) Logout() error {
	if err := c.clearSession(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(c.ConfigDir, "token.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return configError("Cannot remove the saved credentials.")
	}
	c.Token = Token{}
	c.jar = newSessionJar()
	c.HTTP.Jar = c.jar
	return nil
}

func (c *Client) Status(ctx context.Context, check bool) (AuthStatus, error) {
	status := AuthStatus{
		Authenticated: c.Token.RefreshToken != "" || c.Token.AccessToken != "" && (c.Token.ExpiredAt.IsZero() || time.Now().Before(c.Token.ExpiredAt)),
		ConfigDir:     c.ConfigDir,
		ExpiresAt:     c.Token.ExpiredAt,
	}
	if check {
		user, err := c.User(ctx)
		if err != nil {
			return status, err
		}
		status.Authenticated = true
		status.ExpiresAt = c.Token.ExpiredAt
		status.User = &user
	}
	return status, nil
}
