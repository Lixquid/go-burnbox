// Cross-platform trashing. Every expired entry is moved to the system
// trash: the freedesktop.org Trash on Linux, the Recycle Bin on
// Windows, and the macOS Trash on Darwin. The platform mechanisms live
// in trash_linux.go, trash_windows.go and trash_darwin.go; this file
// holds the shared plumbing.

package main

// trashInFolder moves a top-level entry to the system trash. It never
// falls back to deleting here; callers decide what to do on failure.
func trashInFolder(cfg Config, name string) error {
	src, err := verifiedPath(cfg, name)
	if err != nil {
		return err
	}
	return sendToTrash(src)
}
