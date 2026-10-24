//go:build darwin

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// sendToTrash sends path to the macOS Trash by asking Finder to delete
// it (AppleScript "delete"). This is the supported way to trash an item:
// it lands in the Trash, and it can be restored with "Put Back". It
// never falls back to deleting.
func sendToTrash(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	// Escape for an AppleScript string literal.
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(abs)
	script := `tell application "Finder" to delete POSIX file "` + quoted + `"`

	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot move %q to the Trash: %w (%s)",
			abs, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureTrashAvailable reports that the macOS Trash is usable. The Trash
// always exists (Finder creates it on first use), so this only verifies
// that osascript, through which trashing is performed, is available.
func ensureTrashAvailable() (string, error) {
	if _, err := exec.LookPath("osascript"); err != nil {
		return "", fmt.Errorf("osascript not available: %w", err)
	}
	return "Trash", nil
}
