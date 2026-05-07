package util

import (
	"path/filepath"
	"strings"
)

// StripExtension removes the file extension from a filename
func StripExtension(name string) string {
	if dot := strings.LastIndex(name, "."); dot != -1 {
		return name[:dot]
	}
	return name
}

// IsVideoFilename returns true when a filename has a known video extension.
func IsVideoFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v", ".3gp":
		return true
	default:
		return false
	}
}
