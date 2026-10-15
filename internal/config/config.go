// Package config reads the optional dispatch config file.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// File is ~/.dispatch/config.json. Unknown keys are ignored.
type File struct {
	Priority string `json:"priority,omitempty"`
}

// Path is the config file inside a dispatch home directory.
func Path(home string) string { return filepath.Join(home, "config.json") }

// Load reads path. A missing or empty file is a zero config, not an error.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return File{}, nil
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	switch f.Priority {
	case "", "speed", "balanced", "quality":
		return f, nil
	default:
		return File{}, fmt.Errorf("%s: invalid priority %q (want speed|balanced|quality)", path, f.Priority)
	}
}
