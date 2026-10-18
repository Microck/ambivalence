package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Microck/ambivalence/internal/protocol"
)

// File is the agent's saved state. Device is the linked phone's public id; Key is the link key
// from pairing (hex), the only secret, so the file is written 0600.
type File struct {
	Hub    string `json:"hub,omitempty"`
	Listen string `json:"listen,omitempty"`
	Device string `json:"device,omitempty"`
	Key    string `json:"key,omitempty"`
}

func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ambivalence", "config.json"), nil
}

func Load() (File, error) {
	var f File
	p, err := Path()
	if err != nil {
		return f, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, err
	}
	return f, nil
}

func Save(f File) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600) // WriteFile keeps the mode of an existing file
}

func HubURL(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("AMBIVALENCE_HUB")); v != "" {
		return v
	}
	f, _ := Load()
	if strings.TrimSpace(f.Hub) != "" {
		return f.Hub
	}
	return "ws://127.0.0.1:7842" + protocol.Path
}

func ListenAddr(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("AMBIVALENCE_LISTEN")); v != "" {
		return v
	}
	f, _ := Load()
	if strings.TrimSpace(f.Listen) != "" {
		return f.Listen
	}
	return protocol.DefaultListen
}
