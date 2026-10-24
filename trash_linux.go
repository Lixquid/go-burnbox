//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sendToTrash moves path into the freedesktop.org Trash: the entry goes
// to Trash/files under a possibly disambiguated name, and a matching
// .trashinfo record recording the original path and deletion date is
// written to Trash/info. It never falls back to deleting.
func sendToTrash(path string) error {
	filesDir, infoDir, err := resolveTrashDirs()
	if err != nil {
		return err
	}
	for _, d := range []string{filesDir, infoDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("cannot create trash dir %s: %w", d, err)
		}
	}

	dst := uniqueTrashName(filesDir, filepath.Base(path))
	if err := os.Rename(path, dst); err != nil {
		return fmt.Errorf("cannot move to trash: %w", err)
	}

	infoPath := filepath.Join(infoDir, filepath.Base(dst)+".trashinfo")
	info := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		path, time.Now().Format("2006-01-02T15:04:05"))
	if err := os.WriteFile(infoPath, []byte(info), 0o600); err != nil {
		// The item is already in the trash; a missing .trashinfo is
		// harmless (some file managers may show it as orphaned).
		log.Printf("warning: could not write trashinfo for %q: %v", path, err)
	}
	return nil
}

// resolveTrashDirs returns the trash "files" and "info" directories,
// following the freedesktop.org Trash specification.
func resolveTrashDirs() (filesDir, infoDir string, err error) {
	trashDir := os.Getenv("XDG_DATA_HOME")
	if trashDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("cannot find home directory: %w", err)
		}
		trashDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(trashDir, "Trash", "files"),
		filepath.Join(trashDir, "Trash", "info"), nil
}

// ensureTrashAvailable checks that the freedesktop.org trash location
// exists or can be created, returning the trash files/ directory on
// success.
func ensureTrashAvailable() (string, error) {
	filesDir, infoDir, err := resolveTrashDirs()
	if err != nil {
		return "", err
	}
	for _, d := range []string{filesDir, infoDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("cannot create trash dir %s: %w", d, err)
		}
	}
	return filesDir, nil
}

// uniqueTrashName returns a path in dir with no existing entry, appending
// ".2", ".3", ... as needed.
func uniqueTrashName(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); err != nil {
			return p // does not exist (or inaccessible); use it
		}
		p = filepath.Join(dir, fmt.Sprintf("%s.%d%s", base, i, ext))
	}
}
