package main

import (
	"bytes"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestMain(m *testing.M) {
	// Keep test output readable; failures are reported via t.Fatal etc.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// --- helpers ---

// newEnv builds a fully isolated environment for one test: a temp watch
// folder, a temp store path, a temp trash location (XDG_DATA_HOME) and a
// matching Config with the given expiration and force settings.
func newEnv(t *testing.T, expiration time.Duration, force bool) (Config, *store) {
	t.Helper()
	dir := t.TempDir()
	watch := filepath.Join(dir, "watch")
	if err := os.MkdirAll(watch, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "trash-data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))

	cfg := Config{
		Folder:     watch,
		Expiration: expiration,
		Ignore:     []string{".directory"},
		Force:      force,
		StorePath:  filepath.Join(dir, "store.json"),
	}
	st, err := loadStore(cfg.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, st
}

// touch creates an empty file (or dir if isDir) inside watch.
func touch(t *testing.T, watch, name string, isDir bool) string {
	t.Helper()
	p := filepath.Join(watch, name)
	var err error
	if isDir {
		err = os.MkdirAll(p, 0o755)
	} else {
		err = os.WriteFile(p, []byte("x"), 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// trackedOnDisk re-reads the store from disk and returns the first-seen
// time of a name. The daemon runs with its own in-memory store instance,
// so tests must observe persisted state.
func trackedOnDisk(cfg Config, name string) (time.Time, bool) {
	st, err := loadStore(cfg.StorePath)
	if err != nil {
		return time.Time{}, false
	}
	return st.peekFirstSeen(name)
}

// startJanitor runs runJanitor in the background with a stop channel that
// is closed exactly once at test cleanup.
func startJanitor(t *testing.T, cfg Config) *sync.Once {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	shutdown := func() {
		once.Do(func() { close(stop) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("runJanitor did not stop in time")
		}
	}
	t.Cleanup(shutdown)
	go func() { done <- runJanitor(cfg, stop) }()
	return &once
}

// waitFor polls cond until true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// --- store tests ---

func TestStoreSeenKeepsOriginalFirstSeenTime(t *testing.T) {
	_, st := newEnv(t, time.Hour, false)
	t1 := time.Now().Add(-time.Hour)
	if err := st.seen("a.txt", t1); err != nil {
		t.Fatal(err)
	}
	// Seeing the same name again must NOT overwrite the first-seen time.
	if err := st.seen("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, ok := st.peekFirstSeen("a.txt")
	if !ok {
		t.Fatal("a.txt not tracked")
	}
	if !got.Equal(t1) {
		t.Fatalf("first-seen time overwritten: want %v, got %v", t1, got)
	}
}

func TestStorePersistsAcrossRestart(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	t1 := time.Now().Add(-30 * time.Minute)
	if err := st.seen("a.txt", t1); err != nil {
		t.Fatal(err)
	}
	if err := st.seen("b.txt", time.Now()); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: reload from disk into a fresh store.
	st2, err := loadStore(cfg.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st2.peekFirstSeen("a.txt")
	if !ok {
		t.Fatal("a.txt lost after restart")
	}
	if !got.Equal(t1) {
		t.Fatalf("timer did not survive restart: want %v, got %v", t1, got)
	}
	if _, ok := st2.peekFirstSeen("b.txt"); !ok {
		t.Fatal("b.txt lost after restart")
	}
}

func TestStoreForget(t *testing.T) {
	_, st := newEnv(t, time.Hour, false)
	if err := st.seen("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.forget("a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.peekFirstSeen("a.txt"); ok {
		t.Fatal("a.txt still tracked after forget")
	}
	// Forgetting again / unknown names must not error.
	if err := st.forget("a.txt"); err != nil {
		t.Fatalf("second forget errored: %v", err)
	}
	if err := st.forget("never-seen.txt"); err != nil {
		t.Fatalf("forget unknown errored: %v", err)
	}
}

func TestStoreExpired(t *testing.T) {
	_, st := newEnv(t, 500*time.Millisecond, false)
	if err := st.seen("old.txt", time.Now().Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.seen("recent.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	exp := st.expired(500*time.Millisecond, time.Now())
	if _, ok := exp["old.txt"]; !ok {
		t.Fatal("old.txt should have expired")
	}
	if _, ok := exp["recent.txt"]; ok {
		t.Fatal("recent.txt should not have expired")
	}
}

func TestStorePrune(t *testing.T) {
	_, st := newEnv(t, time.Hour, false)
	if err := st.seen("kept.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.seen("gone.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.prune(map[string]bool{"kept.txt": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.peekFirstSeen("gone.txt"); ok {
		t.Fatal("gone.txt should have been pruned")
	}
	if _, ok := st.peekFirstSeen("kept.txt"); !ok {
		t.Fatal("kept.txt should have survived pruning")
	}
	// Pruning must persist to disk.
	st2, err := loadStore(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2.peekFirstSeen("gone.txt"); ok {
		t.Fatal("prune was not persisted")
	}
}

func TestLoadStoreMissingFileIsEmpty(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	st, err := loadStore(cfg.StorePath)
	if err != nil {
		t.Fatalf("missing store file should not error: %v", err)
	}
	if len(st.peekAll()) != 0 {
		t.Fatal("fresh store should be empty")
	}
}

func TestLoadStoreCorruptFileErrors(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	if err := os.WriteFile(cfg.StorePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStore(cfg.StorePath); err == nil {
		t.Fatal("expected error for corrupt store")
	}
}

// --- config tests ---

func TestParseFlagsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg, err := parseFlags(fs, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Expiration != 24*time.Hour {
		t.Fatalf("default expiration: want 24h, got %s", cfg.Expiration)
	}
	if filepath.Base(cfg.Folder) != "BurnBox" {
		t.Fatalf("default folder should end in BurnBox, got %q", cfg.Folder)
	}
	if len(cfg.Ignore) != 1 || cfg.Ignore[0] != ".directory" {
		t.Fatalf("default ignore list: want [.directory], got %v", cfg.Ignore)
	}
	if cfg.Force {
		t.Fatal("default force should be false")
	}
	if cfg.StorePath == "" {
		t.Fatal("default store path must not be empty")
	}
	// Default store must live OUTSIDE the watched folder.
	if strings.HasPrefix(cfg.StorePath, cfg.Folder+string(filepath.Separator)) {
		t.Fatalf("default store %q must not be inside watch folder %q", cfg.StorePath, cfg.Folder)
	}
	if !strings.Contains(cfg.StorePath, "BurnBox") {
		t.Fatalf("default store should live under a BurnBox cache dir: %q", cfg.StorePath)
	}
}

func TestParseFlagsExplicit(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg, err := parseFlags(fs, []string{
		"-folder", "/tmp/watchme",
		"-expiration", "5m",
		"-ignore", "a.txt, .b , ,c",
		"-force",
		"-store", "/tmp/store.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Folder != "/tmp/watchme" {
		t.Fatalf("folder: got %q", cfg.Folder)
	}
	if cfg.Expiration != 5*time.Minute {
		t.Fatalf("expiration: got %s", cfg.Expiration)
	}
	if !cfg.Force {
		t.Fatal("force should be true")
	}
	if cfg.StorePath != "/tmp/store.json" {
		t.Fatalf("store: got %q", cfg.StorePath)
	}
	// Blank segments are dropped; whitespace trimmed.
	want := []string{"a.txt", ".b", "c"}
	if len(cfg.Ignore) != len(want) {
		t.Fatalf("ignore list: want %v, got %v", want, cfg.Ignore)
	}
	for i := range want {
		if cfg.Ignore[i] != want[i] {
			t.Fatalf("ignore list: want %v, got %v", want, cfg.Ignore)
		}
	}
}

func TestDefaultStorePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	p1, err := defaultStorePath("/w/one")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := defaultStorePath("/w/two")
	if err != nil {
		t.Fatal(err)
	}
	// Must be inside the user cache dir under a BurnBox subfolder.
	if !strings.HasPrefix(p1, filepath.Join(dir, "BurnBox")+string(filepath.Separator)) {
		t.Fatalf("store path %q not under cache dir %q", p1, dir)
	}
	// Different folders must not clash.
	if p1 == p2 {
		t.Fatalf("different folders map to same store: %s", p1)
	}
	// Same folder twice must be stable (restart finds the same store).
	p1b, err := defaultStorePath("/w/one")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p1b {
		t.Fatalf("store path unstable: %q vs %q", p1, p1b)
	}
	// Ugly folder names are sanitized into the filename.
	p3, err := defaultStorePath("/w/my weird folder name")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(p3)
	if strings.ContainsAny(base, " \\/") {
		t.Fatalf("store filename not sanitized: %q", base)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"clean":      "clean",
		"my dir":     "my-dir",
		"with/slash": "with-slash",
		"":           "folder",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Fatalf("sanitizeName(%q): want %q, got %q", in, want, got)
		}
	}
}

// --- path safety tests ---

func TestVerifiedPathAcceptsTopLevelNames(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	p, err := verifiedPath(cfg, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(cfg.Folder, "a.txt") {
		t.Fatalf("verified path: got %q", p)
	}
}

func TestVerifiedPathRejectsEscapes(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	bad := []string{
		"..",             // parent itself
		".",              // the folder itself
		"a/b.txt",        // nested: never touches non-top-level items
		"sub/../a.txt",   // contains a separator
		"/etc/passwd",    // absolute path
		"../outside.txt", // escape attempt
		"../..",          // nested parent escape
	}
	for _, name := range bad {
		if _, err := verifiedPath(cfg, name); err == nil {
			t.Fatalf("verifiedPath(%q) should have been rejected", name)
		}
	}
}

func TestIsStoreFile(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	if !isStoreFile(cfg, filepath.Base(cfg.StorePath)) {
		t.Fatal("store file itself should match")
	}
	if !isStoreFile(cfg, filepath.Base(cfg.StorePath)+".tmp") {
		t.Fatal("store temp file should match")
	}
	if isStoreFile(cfg, "other.txt") {
		t.Fatal("unrelated file should not match")
	}
}

func TestShouldIgnore(t *testing.T) {
	cfg := Config{Ignore: []string{".directory", "keep.me"}}
	if !shouldIgnore(cfg, ".directory") || !shouldIgnore(cfg, "keep.me") {
		t.Fatal("ignored names should match")
	}
	if shouldIgnore(cfg, "other.txt") {
		t.Fatal("unlisted name should not be ignored")
	}
	if shouldIgnore(Config{}, "anything") {
		t.Fatal("empty ignore list should never match")
	}
}

// --- event handling tests ---

func TestHandleEventCreateTracksInStore(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	ev := fsnotify.Event{Name: filepath.Join(cfg.Folder, "a.txt"), Op: fsnotify.Create}
	handleEvent(cfg, st, ev)
	if _, ok := st.peekFirstSeen("a.txt"); !ok {
		t.Fatal("created file should be tracked in the store")
	}
}

func TestHandleEventCreateIgnoresListedNames(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	ev := fsnotify.Event{Name: filepath.Join(cfg.Folder, ".directory"), Op: fsnotify.Create}
	handleEvent(cfg, st, ev)
	if _, ok := st.peekFirstSeen(".directory"); ok {
		t.Fatal("ignored name must not be tracked")
	}
}

func TestHandleEventCreateIgnoresStoreFile(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	// Create events for the store file and its temp file must be dropped,
	// otherwise saving the store feeds back into itself forever.
	for _, n := range []string{filepath.Base(cfg.StorePath), filepath.Base(cfg.StorePath) + ".tmp"} {
		ev := fsnotify.Event{Name: filepath.Join(cfg.Folder, n), Op: fsnotify.Create}
		handleEvent(cfg, st, ev)
	}
	if len(st.peekAll()) != 0 {
		t.Fatalf("store files must never be tracked, got %v", st.peekAll())
	}
}

func TestHandleEventCreateIgnoresEventsOutsideFolder(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	outside := filepath.Join(filepath.Dir(cfg.Folder), "elsewhere.txt")
	ev := fsnotify.Event{Name: outside, Op: fsnotify.Create}
	handleEvent(cfg, st, ev)
	if len(st.peekAll()) != 0 {
		t.Fatalf("events outside the watch folder must be ignored, got %v", st.peekAll())
	}
}

func TestHandleEventRemoveForgets(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	if err := st.seen("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	ev := fsnotify.Event{Name: filepath.Join(cfg.Folder, "a.txt"), Op: fsnotify.Remove}
	handleEvent(cfg, st, ev)
	if _, ok := st.peekFirstSeen("a.txt"); ok {
		t.Fatal("removed file must no longer be tracked")
	}
}

func TestHandleEventRenameForgets(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	if err := st.seen("old.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	ev := fsnotify.Event{Name: filepath.Join(cfg.Folder, "old.txt"), Op: fsnotify.Rename}
	handleEvent(cfg, st, ev)
	if _, ok := st.peekFirstSeen("old.txt"); ok {
		t.Fatal("renamed-away file must no longer be tracked")
	}
}

// --- expiry handling tests ---

func TestHandleExpiredForceDeletes(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, true) // force = true
	p := touch(t, cfg.Folder, "doomed.txt", false)
	if err := st.seen("doomed.txt", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	handleExpired(cfg, st, "doomed.txt", time.Now().Add(-2*time.Hour))
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted, lstat err: %v", err)
	}
	if _, ok := st.peekFirstSeen("doomed.txt"); ok {
		t.Fatal("deleted file must be removed from the store")
	}
}

func TestHandleExpiredAlreadyGone(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)
	if err := st.seen("phantom.txt", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// File does not exist at all; must be handled without error and
	// dropped from the store.
	handleExpired(cfg, st, "phantom.txt", time.Now().Add(-2*time.Hour))
	if _, ok := st.peekFirstSeen("phantom.txt"); ok {
		t.Fatal("phantom entry must be forgotten")
	}
}

// --- daemon integration tests ---

// TestRunJanitorStartupSync: on startup, pre-existing files keep their
// stored first-seen time, and store entries for files that vanished while
// the daemon was down are pruned.
func TestRunJanitorStartupSync(t *testing.T) {
	cfg, st := newEnv(t, time.Hour, false)

	// Pre-seed the store as if a previous run had seen these.
	known := time.Now().Add(-time.Hour)
	if err := st.seen("known.txt", known); err != nil {
		t.Fatal(err)
	}
	if err := st.seen("vanished.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	// known.txt exists on disk; vanished.txt does not.
	touch(t, cfg.Folder, "known.txt", false)

	startJanitor(t, cfg)

	// known.txt keeps its original first-seen time.
	waitFor(t, 3*time.Second, "startup sync to keep known.txt's first-seen time", func() bool {
		got, ok := trackedOnDisk(cfg, "known.txt")
		return ok && got.Equal(known)
	})
	// vanished.txt is pruned from the store.
	waitFor(t, 3*time.Second, "vanished.txt to be pruned", func() bool {
		_, ok := trackedOnDisk(cfg, "vanished.txt")
		return !ok
	})
}

// TestRunJanitorStartupSyncNewFileSeenNow: a pre-existing file that the
// daemon has never seen gets "now" as its first-seen time — file
// properties (mod time etc.) must be irrelevant.
func TestRunJanitorStartupSyncNewFileSeenNow(t *testing.T) {
	cfg, _ := newEnv(t, time.Hour, false)
	p := touch(t, cfg.Folder, "brandnew.txt", false)
	// Backdate the file's mod time far into the past: expiry must NOT
	// use this value.
	old := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	startJanitor(t, cfg)

	waitFor(t, 3*time.Second, "brandnew.txt to be tracked", func() bool {
		_, ok := trackedOnDisk(cfg, "brandnew.txt")
		return ok
	})
	got, _ := trackedOnDisk(cfg, "brandnew.txt")
	// First seen must be ~now (well after start), not the year-old mod time.
	if got.Sub(old) < 300*24*time.Hour {
		t.Fatalf("first-seen must not be derived from mod time: got %v (mod time %v)", got, old)
	}
	if got.Before(start.Add(-5 * time.Second)) {
		t.Fatalf("first-seen should be ~now, got %v", got)
	}
}

// --- CLI routing tests ---

func TestRunNoSubcommand(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(nil, stdout, stderr)
	if code != 2 {
		t.Fatalf("no subcommand: want exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("no subcommand: usage should be printed, got %q", stderr.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run([]string{"frobnicate"}, stdout, stderr)
	if code != 2 {
		t.Fatalf("unknown subcommand: want exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") || !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("unknown subcommand: want error and usage on stderr, got %q", stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := run([]string{arg}, stdout, stderr)
		if code != 0 {
			t.Fatalf("%q: want exit code 0, got %d", arg, code)
		}
		if !strings.Contains(stdout.String(), "start") {
			t.Fatalf("%q: usage should mention the start subcommand, got %q", arg, stdout.String())
		}
	}
}

func TestRunStartHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := run([]string{"start", arg}, stdout, stderr)
		if code != 0 {
			t.Fatalf("start %q: want exit code 0, got %d", arg, code)
		}
		out := stdout.String()
		if !strings.Contains(out, "Usage: ") || !strings.Contains(out, "daemon") || !strings.Contains(out, "-folder") || !strings.Contains(out, "-expiration") {
			t.Fatalf("start %q: want start usage with description and flag list, got %q", arg, out)
		}
	}
}
