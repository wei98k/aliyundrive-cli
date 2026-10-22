package drive

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxConfigBytes = 1 << 20

func defaultConfigDir() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		if !filepath.IsAbs(base) {
			return "", Invalid("XDG_CONFIG_HOME must be an absolute path")
		}
		return filepath.Join(base, "aliyundrive"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", configError("Cannot determine the configuration directory.")
	}
	return filepath.Join(home, ".config", "aliyundrive"), nil
}

func configError(message string) *Error {
	return E("IO_ERROR", message, "Check the configuration directory and file permissions.", 7)
}

func secureDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return configError("The configuration path must be a real directory.")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return configError("Cannot inspect the configuration directory.")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return configError("Cannot create the configuration directory.")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return configError("Cannot secure the configuration directory.")
	}
	return nil
}

func readPrivateJSON(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return configError("Cannot read the saved authentication state.")
	}
	if !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return configError("The saved authentication state is not a valid regular file.")
	}
	if info.Mode().Perm()&0077 != 0 {
		return configError("The saved authentication state is not private; set its permissions to 0600.")
	}
	f, err := os.Open(path)
	if err != nil {
		return configError("Cannot read the saved authentication state.")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes || json.Unmarshal(data, out) != nil {
		return configError("The saved authentication state is invalid JSON.")
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return configError("Cannot create a private authentication file.")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return configError("Cannot write the authentication state.")
	}
	if err := os.Rename(name, path); err != nil {
		return configError("Cannot replace the saved authentication state.")
	}
	return nil
}

func writePrivateJSON(path string, data any) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return configError("Cannot encode the authentication state.")
	}
	return writePrivate(path, append(b, '\n'))
}

func validToken(s string) bool {
	return len(s) > 0 && len(s) <= 16384 && !strings.ContainsAny(s, " \t\r\n\x00")
}

func (c *Client) saveToken() error {
	return writePrivateJSON(filepath.Join(c.ConfigDir, "token.json"), c.Token)
}
