package drive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func assertPrivateAuthFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("%s permissions=%o", filepath.Base(path), info.Mode().Perm())
	}
}

func TestNewClientDefaultConfigAndEnvironment(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "environment-refresh")
	c, err := NewClient("", time.Second)
	if err != nil || c.ConfigDir != filepath.Join(base, "aliyundrive") || c.Token.RefreshToken != "environment-refresh" {
		t.Fatalf("client=%#v error=%v", c, err)
	}
	status, err := c.Status(context.Background(), false)
	if err != nil || !status.Authenticated || status.User != nil {
		t.Fatalf("status=%#v error=%v", status, err)
	}
	if _, err := os.Stat(c.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only creation unexpectedly changed disk: %v", err)
	}
}

func TestPrivateConfigRejectsSymlinkAndWorldReadableFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	if err := writePrivateJSON(file, Token{RefreshToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	requireDriveError(t, readPrivateJSON(link, &Token{}), "IO_ERROR", 7)
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	requireDriveError(t, readPrivateJSON(file, &Token{}), "IO_ERROR", 7)
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0644 {
		t.Error("read unexpectedly changed permissions")
	}
}

func TestAtomicTokenWriteReplacesPrivateFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new-config")
	path := filepath.Join(dir, "token.json")
	if err := writePrivateJSON(path, Token{RefreshToken: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(path, Token{RefreshToken: "second"}); err != nil {
		t.Fatal(err)
	}
	var token Token
	if err := readPrivateJSON(path, &token); err != nil || token.RefreshToken != "second" {
		t.Fatalf("token=%#v error=%v", token, err)
	}
	assertPrivateAuthFile(t, path)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "token.json" {
		t.Fatalf("temporary credentials left behind: %#v", entries)
	}
}

func TestNewAuthClientAllowsRepairingCorruptSavedCredentials(t *testing.T) {
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "invalid environment token")
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := writePrivate(path, []byte("broken credentials")); err != nil {
		t.Fatal(err)
	}
	_, err := NewClient(dir, time.Second)
	requireDriveError(t, err, "INVALID_ARGUMENT", 2)
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "")
	_, err = NewClient(dir, time.Second)
	requireDriveError(t, err, "IO_ERROR", 7)
	t.Setenv("ALIYUNDRIVE_REFRESH_TOKEN", "invalid environment token")
	c, err := NewAuthClient(dir, time.Second)
	if err != nil || c.Token.RefreshToken != "" {
		t.Fatalf("recovery client error=%v", err)
	}
	if err := c.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("recovery logout did not remove corrupt credentials")
	}
}
