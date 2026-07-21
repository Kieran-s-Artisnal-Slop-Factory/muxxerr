package main

import (
	"path/filepath"
	"strings"
)

func fileDir(p string) string { return filepath.Dir(p) }

func joinLines(items []string) string { return strings.Join(items, "\n  - ") }

// portOf extracts the ":8080" part of a listen address for the friendly URL
// printed at boot. An address with no host is already in that form.
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
