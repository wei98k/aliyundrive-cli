package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

type Client struct {
	HTTP         *http.Client
	APIBase      string
	AuthBase     string
	PassportBase string
	ConfigDir    string
	DriveID      string
	Token        Token
	jar          *sessionJar
}

func NewClient(configDir string, timeout time.Duration) (*Client, error) {
	return newClient(configDir, timeout, true)
}

// NewAuthClient permits explicit recovery operations even when saved credentials
// are corrupt, insecure, or overridden by an invalid environment variable.
func NewAuthClient(configDir string, timeout time.Duration) (*Client, error) {
	return newClient(configDir, timeout, false)
}

func newClient(configDir string, timeout time.Duration, loadCredentials bool) (*Client, error) {
	if configDir == "" {
		var err error
		configDir, err = defaultConfigDir()
		if err != nil {
			return nil, err
		}
	}
	var err error
	configDir, err = filepath.Abs(configDir)
	if err != nil {
		return nil, configError("Cannot resolve the configuration directory.")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	jar := newSessionJar()
	c := &Client{
		HTTP:         &http.Client{Timeout: timeout, Jar: jar},
		APIBase:      "https://api.aliyundrive.com",
		AuthBase:     "https://auth.aliyundrive.com",
		PassportBase: "https://passport.aliyundrive.com",
		ConfigDir:    configDir,
		jar:          jar,
	}
	if !loadCredentials {
		return c, nil
	}
	if token := strings.TrimSpace(os.Getenv("ALIYUNDRIVE_REFRESH_TOKEN")); token != "" {
		if !validToken(token) {
			return nil, Invalid("ALIYUNDRIVE_REFRESH_TOKEN is not a valid token")
		}
		c.Token = Token{RefreshToken: token}
		return c, nil
	}
	if err := readPrivateJSON(filepath.Join(configDir, "token.json"), &c.Token); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if c.Token.AccessToken != "" && !validToken(c.Token.AccessToken) || c.Token.RefreshToken != "" && !validToken(c.Token.RefreshToken) {
		return nil, configError("The saved authentication state contains an invalid token.")
	}
	return c, nil
}

func authRequired() *Error {
	return E("AUTH_REQUIRED", "Authentication is required.", "Run aliyundrive auth login, or import a refresh token with aliyundrive auth import --stdin.", 3)
}

func canceledError() *Error {
	return E("CANCELED", "The operation was canceled.", "", 8)
}

func networkError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return canceledError()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return E("TIMEOUT", "The operation exceeded its deadline.", "Increase --timeout. A write may have completed; check before retrying.", 8)
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		e := E("TIMEOUT", "The request timed out.", "Increase --timeout or try again. A write may have completed; check before retrying.", 6)
		e.Retryable = true
		return e
	}
	e := E("NETWORK_ERROR", "Cannot reach the Aliyun Drive service.", "Check your network. A write may have completed; check before retrying.", 6)
	e.Retryable = true
	return e
}

func apiError(status int, data []byte) error {
	var remote struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &remote)
	code := strings.ToLower(remote.Code)
	if status == http.StatusUnauthorized || strings.Contains(code, "tokeninvalid") || strings.Contains(code, "tokenexpired") || strings.Contains(code, "invalidaccesstoken") || strings.Contains(code, "invalidtoken") || strings.Contains(code, "refreshtoken") {
		return E("AUTH_REQUIRED", "The saved authentication is invalid or expired.", "Run aliyundrive auth login, or import a new refresh token.", 3)
	}
	if status == http.StatusNotFound || strings.Contains(code, "notfound") || strings.Contains(code, "notexist") {
		return E("NOT_FOUND", "The requested file or resource was not found.", "Check the remote path or file ID.", 4)
	}
	if status == http.StatusConflict || strings.Contains(code, "alreadyexist") || strings.Contains(code, "conflict") {
		return E("CONFLICT", "The operation conflicts with an existing resource.", "Choose another name or destination.", 5)
	}
	if status == http.StatusForbidden {
		return E("PERMISSION_DENIED", "The service denied this operation.", "Check account and file permissions.", 6)
	}
	e := E("API_ERROR", fmt.Sprintf("Aliyun Drive returned an API error (HTTP %d).", status), "", 6)
	if status == http.StatusTooManyRequests || status >= 500 {
		e.Retryable = true
	}
	return e
}

// do never retries, and deliberately excludes response bodies and URLs from errors.
func (c *Client) do(ctx context.Context, method, target, contentType string, body []byte, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return E("API_ERROR", "Cannot construct the API request.", "", 6)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "aliyundrive-cli/1.0")
	req.Header.Set("Referer", "https://www.aliyundrive.com/")
	req.Header.Set("Origin", "https://www.aliyundrive.com")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := *c.HTTP
	previousRedirectPolicy := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !c.allowedLoginURL(next.URL) {
			return http.ErrUseLastResponse
		}
		if (bearer != "" || len(body) != 0) && (next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host) {
			return http.ErrUseLastResponse
		}
		if previousRedirectPolicy != nil {
			return previousRedirectPolicy(next, via)
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return networkError(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return networkError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return E("API_ERROR", "The service response exceeded the size limit.", "", 6)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, data)
	}
	if resp.StatusCode == http.StatusNoContent && len(data) == 0 {
		return nil
	}
	// Some API gateways report an application error in a successful HTTP response.
	if len(data) != 0 && json.Valid(data) {
		var envelope struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Code != "" && envelope.Code != "200" && !strings.EqualFold(envelope.Code, "success") {
			return apiError(resp.StatusCode, data)
		}
	}
	if out != nil && (len(data) == 0 || json.Unmarshal(data, out) != nil) {
		return E("API_ERROR", "The service returned an invalid JSON response.", "", 6)
	}
	return nil
}

func (c *Client) jsonRequest(ctx context.Context, method, target string, body any, bearer string, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return Invalid("The request body cannot be encoded as JSON")
	}
	return c.do(ctx, method, target, "application/json", data, bearer, out)
}

func (c *Client) Request(ctx context.Context, endpoint string, body any, out any) error {
	if !strings.HasPrefix(endpoint, "/") || strings.HasPrefix(endpoint, "//") || strings.ContainsAny(endpoint, "?#\\") {
		return Invalid("API endpoint must be a slash-prefixed path")
	}
	refreshed := c.needsRefresh()
	if err := c.ensureAuthenticated(ctx); err != nil {
		return err
	}
	err := c.jsonRequest(ctx, http.MethodPost, strings.TrimRight(c.APIBase, "/")+endpoint, body, c.Token.AccessToken, out)
	var remoteErr *Error
	if !refreshed && errors.As(err, &remoteErr) && remoteErr.Code == "AUTH_REQUIRED" && c.Token.RefreshToken != "" {
		if err := c.refresh(ctx, true); err != nil {
			return err
		}
		return c.jsonRequest(ctx, http.MethodPost, strings.TrimRight(c.APIBase, "/")+endpoint, body, c.Token.AccessToken, out)
	}
	return err
}

func (c *Client) needsRefresh() bool {
	return c.Token.AccessToken == "" || !c.Token.ExpiredAt.IsZero() && time.Until(c.Token.ExpiredAt) <= 30*time.Second
}

func (c *Client) ensureAuthenticated(ctx context.Context) error {
	if c.needsRefresh() {
		return c.refresh(ctx, true)
	}
	return nil
}

type tokenResponse struct {
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	ExpiresIn      int       `json:"expires_in"`
	ExpireTime     time.Time `json:"expire_time"`
	DefaultDriveID string    `json:"default_drive_id"`
}

func (r tokenResponse) token(previous string) (Token, error) {
	if !validToken(r.AccessToken) {
		return Token{}, E("API_ERROR", "The authentication response contained no valid access token.", "", 6)
	}
	if r.RefreshToken == "" {
		r.RefreshToken = previous
	}
	if !validToken(r.RefreshToken) {
		return Token{}, E("API_ERROR", "The authentication response contained no valid refresh token.", "", 6)
	}
	expires := r.ExpireTime
	if r.ExpiresIn > 0 && r.ExpiresIn <= 365*24*60*60 {
		expires = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	if expires.IsZero() {
		expires = time.Now().Add(time.Hour)
	}
	return Token{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiredAt: expires, DriveID: r.DefaultDriveID}, nil
}

func (c *Client) refresh(ctx context.Context, persist bool) error {
	if !validToken(c.Token.RefreshToken) {
		return authRequired()
	}
	var response tokenResponse
	err := c.jsonRequest(ctx, http.MethodPost, strings.TrimRight(c.APIBase, "/")+"/token/refresh", map[string]string{"refresh_token": c.Token.RefreshToken}, "", &response)
	if err != nil {
		return err
	}
	token, err := response.token(c.Token.RefreshToken)
	if err != nil {
		return err
	}
	if token.DriveID == "" {
		token.DriveID = c.Token.DriveID
	}
	previous := c.Token
	c.Token = token
	if persist {
		if err := c.saveToken(); err != nil {
			c.Token = previous
			return err
		}
	}
	return nil
}

func (c *Client) User(ctx context.Context) (User, error) {
	var user User
	if err := c.Request(ctx, "/v2/user/get", struct{}{}, &user); err != nil {
		return User{}, err
	}
	if user.UserID == "" {
		return User{}, E("API_ERROR", "The service returned an invalid user profile.", "", 6)
	}
	return user, nil
}

// userWithToken validates a newly exchanged token without refreshing or saving
// it. Import/login must not overwrite an existing account on validation failure.
func (c *Client) userWithToken(ctx context.Context) (User, error) {
	var user User
	if err := c.jsonRequest(ctx, http.MethodPost, strings.TrimRight(c.APIBase, "/")+"/v2/user/get", struct{}{}, c.Token.AccessToken, &user); err != nil {
		return User{}, err
	}
	if user.UserID == "" {
		return User{}, E("API_ERROR", "The service returned an invalid user profile.", "", 6)
	}
	return user, nil
}

func (c *Client) EnsureDrive(ctx context.Context) (string, error) {
	if err := c.ensureAuthenticated(ctx); err != nil {
		return "", err
	}
	if c.DriveID != "" {
		return c.DriveID, nil
	}
	if c.Token.DriveID != "" {
		return c.Token.DriveID, nil
	}
	user, err := c.User(ctx)
	if err != nil {
		return "", err
	}
	driveID := user.DefaultDriveID
	if driveID == "" {
		driveID = user.ResourceDriveID
	}
	if driveID == "" {
		driveID = user.BackupDriveID
	}
	if driveID == "" {
		return "", E("API_ERROR", "The account has no available drive.", "Pass --drive-id to choose a drive explicitly.", 6)
	}
	c.Token.DriveID = driveID
	return driveID, nil
}
