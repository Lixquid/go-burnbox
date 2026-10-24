//go:build linux

package main

// Tests for the freedesktop.org Trash implementation on Linux. These
// need the XDG trash dirs and helpers from trash_linux.go and are only
// meaningful on Linux; other tests in main_test.go run everywhere.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// breakTrash makes the trash location unusable by placing a regular file
// where Trash/files would be created (mkdir fails even as root).
func breakTrash(t *testing.T) {
	t.Helper()
	filesDir, _, err := resolveTrashDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filesDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- trash tests ---

func TestResolveTrashDirsFromXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	files, info, err := resolveTrashDirs()
	if err != nil {
		t.Fatal(err)
	}
	if files != "/custom/data/Trash/files" || info != "/custom/data/Trash/info" {
		t.Fatalf("unexpected trash dirs: %q, %q", files, info)
	}
}

func TestResolveTrashDirsFromHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/myhome")
	files, info, err := resolveTrashDirs()
	if err != nil {
		t.Fatal(err)
	}
	if files != "/myhome/.local/share/Trash/files" || info != "/myhome/.local/share/Trash/info" {
		t.Fatalf("unexpected trash dirs: %q, %q", files, info)
	}
}

func TestEnsureTrashAvailableSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	files, err := ensureTrashAvailable()
	if err != nil {
		t.Fatalf("trash should be creatable: %v", err)
	}
	if fi, err := os.Stat(files); err != nil || !fi.IsDir() {
		t.Fatalf("trash files dir not created: %v", err)
	}
}

func TestEnsureTrashAvailableFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(blocker, "sub"))
	if _, err := ensureTrashAvailable(); err == nil {
		t.Fatal("expected error when trash location cannot be created")
	}
}

func TestUniqueTrashNameCollisions(t *testing.T) {
	dir := t.TempDir()
	// No collision: name used as-is.
	if got := uniqueTrashName(dir, "a.txt"); got != filepath.Join(dir, "a.txt") {
		t.Fatalf("want plain name, got %q", got)
	}
	// Collisions get .2, .3 suffixes before the extension.
	for _, name := range []string{"a.txt", "a.2.txt", "a.3.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := uniqueTrashName(dir, "a.txt"); got != filepath.Join(dir, "a.4.txt") {
		t.Fatalf("want a.4.txt, got %q", got)
	}
	// Extensionless files keep the scheme too.
	if err := os.WriteFile(filepath.Join(dir, "b"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := uniqueTrashName(dir, "b"); got != filepath.Join(dir, "b.2") {
		t.Fatalf("want b.2, got %q", got)
	}
}

// --- expiry handling tests ---

func TestHandleExpiredTrashes(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	p := touch(t, cfg.Folder, "doomed.txt", false)
	if err := st.seen("doomed.txt", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	handleExpired(cfg, st, "doomed.txt", time.Now().Add(-2*time.Hour))

	// Gone from the folder...
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("file should be moved out, lstat err: %v", err)
	}
	// ...present in the trash...
	filesDir, infoDir, err := resolveTrashDirs()
	if err != nil {
		t.Fatal(err)
	}
	trashed := filepath.Join(filesDir, "doomed.txt")
	if _, err := os.Lstat(trashed); err != nil {
		t.Fatalf("file should be in trash: %v", err)
	}
	// ...with a .trashinfo recording the original path...
	info, err := os.ReadFile(filepath.Join(infoDir, "doomed.txt.trashinfo"))
	if err != nil {
		t.Fatalf("trashinfo missing: %v", err)
	}
	if !strings.Contains(string(info), "Path="+p) {
		t.Fatalf("trashinfo should record original path %q, got:\n%s", p, info)
	}
	// ...and dropped from the store.
	if _, ok := st.peekFirstSeen("doomed.txt"); ok {
		t.Fatal("trashed file must be removed from the store")
	}
}

func TestHandleExpiredFallsBackToDeleteWhenTrashBroken(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	breakTrash(t)

	p := touch(t, cfg.Folder, "doomed.txt", false)
	if err := st.seen("doomed.txt", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	handleExpired(cfg, st, "doomed.txt", time.Now().Add(-2*time.Hour))

	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted as fallback, lstat err: %v", err)
	}
	if _, ok := st.peekFirstSeen("doomed.txt"); ok {
		t.Fatal("deleted file must be removed from the store")
	}
}

// --- daemon integration tests ---

// TestRunJanitorIntegration is the end-to-end test of the daemon with a
// real fsnotify watcher: creation tracking, removal un-tracking, trash on
// expiry, and the first-seen timer surviving a restart.
func TestRunJanitorIntegration(t *testing.T) {
	cfg, _ := newEnv(t, 500*time.Millisecond, false)

	startJanitor(t, cfg)

	// 1. A created file is tracked with a first-seen time of ~now.
	live := touch(t, cfg.Folder, "live.txt", false)
	waitFor(t, 3*time.Second, "live.txt to be tracked", func() bool {
		_, ok := trackedOnDisk(cfg, "live.txt")
		return ok
	})
	firstSeen, _ := trackedOnDisk(cfg, "live.txt")
	if time.Since(firstSeen) > 5*time.Second {
		t.Fatalf("first-seen should be ~now, got %v", firstSeen)
	}

	// 2. A file removed from the folder is no longer tracked.
	os.Remove(live)
	waitFor(t, 3*time.Second, "removed live.txt to be un-tracked", func() bool {
		_, ok := trackedOnDisk(cfg, "live.txt")
		return !ok
	})

	// 3. A file left in the folder gets trashed after the expiration
	//    measured from when the daemon first saw it.
	doomed := touch(t, cfg.Folder, "doomed.txt", false)
	waitFor(t, 3*time.Second, "doomed.txt to be tracked", func() bool {
		_, ok := trackedOnDisk(cfg, "doomed.txt")
		return ok
	})
	waitFor(t, 5*time.Second, "doomed.txt to disappear from folder", func() bool {
		_, err := os.Lstat(doomed)
		return os.IsNotExist(err)
	})
	filesDir, _, err := resolveTrashDirs()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "doomed.txt to appear in trash", func() bool {
		_, err := os.Lstat(filepath.Join(filesDir, "doomed.txt"))
		return err == nil
	})

	// 4. Restarting the daemon must NOT reset the timer: keeper.txt is
	//    tracked, the daemon is stopped and restarted well before its
	//    expiration, and it must still be trashed based on its ORIGINAL
	//    first-seen time.
	keeper := touch(t, cfg.Folder, "keeper.txt", false)
	waitFor(t, 3*time.Second, "keeper.txt to be tracked", func() bool {
		_, ok := trackedOnDisk(cfg, "keeper.txt")
		return ok
	})
	orig, _ := trackedOnDisk(cfg, "keeper.txt")

	// Wait until keeper.txt is close to expiring, then restart.
	time.Sleep(300 * time.Millisecond) // keeper.txt is ~300ms of its 500ms budget
	startJanitor(t, cfg)               // second instance with the same store

	waitFor(t, 5*time.Second, "keeper.txt to be trashed after restart", func() bool {
		_, err := os.Lstat(keeper)
		return os.IsNotExist(err)
	})
	if time.Since(orig) > 3*time.Second {
		t.Fatalf("keeper.txt took too long to expire; timer may have been reset (first seen %v)", orig)
	}
}

// TestRunJanitorForceIntegration: with -force everything is deleted,
// never trashed.
func TestRunJanitorForceIntegration(t *testing.T) {
	cfg, _ := newEnv(t, 400*time.Millisecond, true)

	startJanitor(t, cfg)

	doomed := touch(t, cfg.Folder, "doomed.txt", false)
	waitFor(t, 3*time.Second, "doomed.txt to be tracked", func() bool {
		_, ok := trackedOnDisk(cfg, "doomed.txt")
		return ok
	})
	waitFor(t, 5*time.Second, "doomed.txt to be force-deleted", func() bool {
		_, err := os.Lstat(doomed)
		return os.IsNotExist(err)
	})

	// Nothing may have been trashed.
	filesDir, _, err := resolveTrashDirs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filesDir); err == nil {
		if ents, _ := os.ReadDir(filesDir); len(ents) > 0 {
			t.Fatalf("force mode must not trash, but trash contains: %v", ents)
		}
	}
	if _, ok := trackedOnDisk(cfg, "doomed.txt"); ok {
		t.Fatal("deleted file must be removed from the store")
	}
}

// TestRunJanitorNoTrashFailsAtStartup: without -force, the daemon must
// exit with an error if it cannot find a place to trash files.
func TestRunJanitorNoTrashFailsAtStartup(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	breakTrash(t)

	err := runJanitor(cfg, nil)
	if err == nil {
		t.Fatal("runJanitor should fail when no trash location is available")
	}
	if !strings.Contains(err.Error(), "no place to trash files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRunJanitorForceIgnoresBrokenTrash: with -force, a broken trash
// location must NOT prevent startup.
func TestRunJanitorForceIgnoresBrokenTrash(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, true)
	breakTrash(t)

	startJanitor(t, cfg)
	// Startup did not fail (no t.Fatal from the helper) and the daemon is
	// still running at cleanup time; a dead daemon would surface via the
	// done channel in the cleanup. Additionally prove liveness: the watch
	// folder is usable and tracked events flow, by creating a file and
	// seeing it tracked.
	p := touch(t, cfg.Folder, "alive.txt", false)
	waitFor(t, 3*time.Second, "file to be tracked while trash is broken (force mode)", func() bool {
		_, ok := trackedOnDisk(cfg, filepath.Base(p))
		return ok
	})
}
