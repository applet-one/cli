// Package auth stores the CLI's Applet login.
package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrNotSignedIn means there is no usable saved login.
	ErrNotSignedIn = errors.New("Not signed in. Run `applet login`.")
	// ErrExpired means the saved login has expired.
	ErrExpired = errors.New("Your saved login has expired. Run `applet login`.")
)

// Credentials are a saved login.
type Credentials struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   string `json:"expires_at"`
}

// Store is the login file, shared with the Node.js CLI.
type Store struct {
	Path string
	Now  func() time.Time
}

// DefaultPath is $XDG_CONFIG_HOME/applet-one/config.json, falling back to
// ~/.config/applet-one/config.json on every platform.
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "applet-one", "config.json"), nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token returns the saved access token.
func (s *Store) Token() (string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", ErrNotSignedIn
	}
	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil || credentials.AccessToken == "" {
		return "", ErrNotSignedIn
	}
	expires, err := time.Parse(time.RFC3339Nano, credentials.ExpiresAt)
	if err != nil || !expires.After(s.now()) {
		return "", ErrExpired
	}
	return credentials.AccessToken, nil
}

// Save writes credentials with owner-only permissions.
func (s *Store) Save(token string, expiresIn time.Duration) error {
	data, err := json.MarshalIndent(Credentials{
		AccessToken: token,
		ExpiresAt:   s.now().Add(expiresIn).UTC().Format("2006-01-02T15:04:05.000Z"),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(s.Path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), s.Path)
}

// Remove deletes the saved login.
func (s *Store) Remove() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
