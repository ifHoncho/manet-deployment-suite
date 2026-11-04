// Package sysctl reads and writes kernel parameters under /proc/sys.
package sysctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is the mount point; tests can point it elsewhere.
var Root = "/proc/sys"

func path(key string) string {
	return filepath.Join(Root, strings.ReplaceAll(key, ".", "/"))
}

// Get reads a parameter, trimmed.
func Get(key string) (string, error) {
	b, err := os.ReadFile(path(key))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Set writes a parameter if it differs. It reports whether it changed.
// A missing key (the interface is gone, or the kernel lacks the option)
// is an error the caller decides about.
func Set(key, value string) (bool, error) {
	cur, err := Get(key)
	if err != nil {
		return false, err
	}
	if cur == value {
		return false, nil
	}
	if err := os.WriteFile(path(key), []byte(value), 0o644); err != nil {
		return false, fmt.Errorf("sysctl %s=%s: %w", key, value, err)
	}
	return true, nil
}
