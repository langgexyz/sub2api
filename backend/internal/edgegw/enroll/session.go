package enroll

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Session is the edge's minimal persisted login state: just the owner's sub2api
// token pair. Everything else is derived — email/uid by parsing the JWT, and
// the seal secret + platforms + edge id fetched from the center at runtime via
// GET /edge/v1/config. Storing only these two means a rotated token (auto- or
// re-login) is the only thing that needs writing back.
type Session struct {
	OwnerAccess  string `json:"owner_access"`
	OwnerRefresh string `json:"owner_refresh"`
}

// DefaultSessionPath returns the per-user session file path:
// os.UserConfigDir()/sub2api-edge/session.json.
func DefaultSessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sub2api-edge", "session.json"), nil
}

// LoadSession reads a Session previously written by SaveSession. A missing file
// returns a zero Session and no error (not-logged-in is not an error).
func LoadSession(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, nil
		}
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// SaveSession writes s as JSON, creating parent dirs 0700 and the file 0600
// (it holds the owner refresh token). Empty tokens clear the file (logout).
func SaveSession(path string, s Session) error {
	if s.OwnerAccess == "" && s.OwnerRefresh == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
