// BurnBox: watches a folder and
// trashes or deletes entries a configurable duration after BurnBox
// first saw them. The first-seen times are persisted in a JSON store so
// timers survive restarts.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Config holds all runtime settings for the janitor.
type Config struct {
	Folder     string        // folder to watch
	Expiration time.Duration // how long after first sight an entry may live
	Ignore     []string      // filenames to never touch
	Force      bool          // force delete (skip trash)
	StorePath  string        // path of the first-seen store (JSON)
}

// store is the persistent data store of files seen by BurnBox,
// mapping a top-level name to the time it was first seen. Expiry is
// always computed from this first-seen time, never from any property
// of the file itself (mod time, ctime, etc.).
type store struct {
	mu        sync.Mutex
	path      string
	firstSeen map[string]time.Time
}

// loadStore reads the store from disk. A missing file yields an empty store.
func loadStore(path string) (*store, error) {
	s := &store{
		path:      path,
		firstSeen: make(map[string]time.Time),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.firstSeen); err != nil {
		return nil, fmt.Errorf("corrupt store %q: %w", path, err)
	}
	return s, nil
}

// save atomically persists the store (write to a temp file, then rename).
func (s *store) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveLocked persists the store; caller must hold s.mu.
func (s *store) saveLocked() error {
	data, err := json.MarshalIndent(s.firstSeen, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// seen records (or keeps) the first-seen time of a name.
func (s *store) seen(name string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.firstSeen[name]; !ok {
		s.firstSeen[name] = at
		return s.saveLocked()
	}
	return nil
}

// forget removes a name from the store entirely (removed from folder,
// renamed away, or handled by the reaper).
func (s *store) forget(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.firstSeen[name]; ok {
		delete(s.firstSeen, name)
		return s.saveLocked()
	}
	return nil
}

// expired returns the entries whose first-seen time is older than d.
func (s *store) expired(d time.Duration, now time.Time) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time)
	for name, at := range s.firstSeen {
		if now.Sub(at) >= d {
			out[name] = at
		}
	}
	return out
}

// prune removes store entries that no longer exist in the watched folder
// (e.g. removed while BurnBox was not running). Returns the set of
// names still in the store after pruning.
func (s *store) prune(present map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for name := range s.firstSeen {
		if !present[name] {
			delete(s.firstSeen, name)
			changed = true
		}
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

// peekFirstSeen returns the stored first-seen time of a name and whether
// it is tracked. Used by tests.
func (s *store) peekFirstSeen(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.firstSeen[name]
	return at, ok
}

// peekAll returns a copy of the whole store. Used by tests.
func (s *store) peekAll() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.firstSeen))
	for k, v := range s.firstSeen {
		out[k] = v
	}
	return out
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run routes a subcommand and returns the process exit code. Split out
// from main so tests can drive the CLI without running the daemon.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		fmt.Fprintln(stderr, "\nerror: no subcommand given")
		return 2
	}
	switch args[0] {
	case "start":
		for _, a := range args[1:] {
			if a == "-h" || a == "--help" {
				printStartUsage(stdout)
				return 0
			}
		}
		cfg, err := parseFlags(flag.CommandLine, args[1:])
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		log.Printf("BurnBox starting: folder=%q expiration=%s ignore=%v force=%t store=%q",
			cfg.Folder, cfg.Expiration, cfg.Ignore, cfg.Force, cfg.StorePath)
		if err := runJanitor(cfg, nil); err != nil {
			log.Fatalf("%v", err)
		}
		return 0
	case "once":
		for _, a := range args[1:] {
			if a == "-h" || a == "--help" {
				printOnceUsage(stdout)
				return 0
			}
		}
		cfg, err := parseFlags(flag.CommandLine, args[1:])
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		log.Printf("BurnBox once: folder=%q expiration=%s ignore=%v force=%t store=%q",
			cfg.Folder, cfg.Expiration, cfg.Ignore, cfg.Force, cfg.StorePath)
		if err := runOnce(cfg); err != nil {
			log.Fatalf("%v", err)
		}
		return 0
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "error: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// printUsage writes the CLI usage information.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %s <command> [flags]

Commands:
  start           start the BurnBox daemon: watch the folder and trash
                  (or delete) entries after their expiration
  status          report the entries in the burnbox and how long they
                  have until deletion
  once            do a single scan of the burnbox now (sync the store,
                  trash or delete expired entries) and exit
  help            show this help

Passing "-h" or "--help" to any subcommand shows usage for that
subcommand.
`, filepath.Base(os.Args[0]))
}

// printOnceUsage writes the usage information for the once subcommand.
func printOnceUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %s once [flags]

Does a single scan of the burnbox immediately, then exits: it records
first-seen times for new entries, prunes entries that have left the
folder, and trashes or deletes every expired entry. It does not watch
the folder afterwards.

Flags:
`, filepath.Base(os.Args[0]))
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	fs.SetOutput(w)
	registerStartFlags(fs)
	fs.PrintDefaults()
}

// printStartUsage writes the usage information for the start subcommand.
func printStartUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %s start [flags]

Starts the BurnBox daemon: watches the folder and
trashes or deletes each entry after its expiration, measured from when
BurnBox first saw it.

Flags:
`, filepath.Base(os.Args[0]))
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(w)
	registerStartFlags(fs)
	fs.PrintDefaults()
}

func registerStartFlags(fs *flag.FlagSet) (folder *string, expire *time.Duration, ignore *string, force *bool, storeP *string) {
	home := "."
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	} else if h, err := os.UserHomeDir(); err == nil && h != "" {
		home = h
	}

	folder = fs.String("folder", filepath.Join(home, "BurnBox"), "folder path to watch")
	expire = fs.Duration("expiration", 24*time.Hour, "expiration duration measured from when BurnBox first saw the file (e.g. 24h, 30m)")
	ignore = fs.String("ignore", ".directory", "comma-separated list of filenames to ignore")
	force = fs.Bool("force", false, "force delete instead of trashing")
	storeP = fs.String("store", "", "path of the first-seen store (default: <user cache dir>/BurnBox/store-<folder>.json)")
	return
}

// parseFlags builds the Config from a flag set and argument list, with
// the required defaults. Split out from main so tests can drive it.
func parseFlags(fs *flag.FlagSet, args []string) (Config, error) {
	folder, expire, ignore, force, storeP := registerStartFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	var ignoreList []string
	for _, s := range strings.Split(*ignore, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			ignoreList = append(ignoreList, s)
		}
	}

	folderPath := *folder
	storePath := *storeP
	if storePath == "" {
		// Default: keep the store out of the watched folder, in the user
		// cache directory, keyed by the watched folder path so multiple
		// instances don't clash.
		p, err := defaultStorePath(folderPath)
		if err != nil {
			return Config{}, fmt.Errorf("cannot determine store location: %w", err)
		}
		storePath = p
	}

	return Config{
		Folder:     folderPath,
		Expiration: *expire,
		Ignore:     ignoreList,
		Force:      *force,
		StorePath:  storePath,
	}, nil
}

// runJanitor is the daemon: it prepares the folder and trash, loads and
// syncs the first-seen store, then watches the folder
// and reaps expired entries. It returns when stop is closed (nil means
// run forever) or when the watcher terminates.
func runJanitor(cfg Config, stop <-chan struct{}) error {
	cfg, err := prepareFolder(cfg)
	if err != nil {
		return err
	}
	log.Printf("watching: %s", cfg.Folder)

	// Load the persistent store of first-seen times.
	st, err := loadStore(cfg.StorePath)
	if err != nil {
		return fmt.Errorf("cannot load store: %w", err)
	}

	syncFolder(cfg, st)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("cannot create watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(cfg.Folder); err != nil {
		return fmt.Errorf("cannot watch %q: %w", cfg.Folder, err)
	}

	// Reaper: periodically expire old entries.
	reapInterval := cfg.Expiration / 4
	if reapInterval < 100*time.Millisecond {
		reapInterval = 100 * time.Millisecond
	}
	if reapInterval > time.Minute {
		reapInterval = time.Minute
	}
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case now := <-ticker.C:
				for name, firstSeen := range st.expired(cfg.Expiration, now) {
					handleExpired(cfg, st, name, firstSeen)
				}
			case <-stop:
				return
			}
		}
	}()

	// Event loop.
	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			handleEvent(cfg, st, ev)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("watcher error: %v", err)

		case <-stop:
			return nil
		}
	}
}

// prepareFolder makes sure the watch folder exists, resolves it to an
// absolute path, and verifies that a trash location is usable unless
// force mode is enabled. It returns the config with the resolved folder.
func prepareFolder(cfg Config) (Config, error) {
	// Make sure the watch folder exists.
	if err := os.MkdirAll(cfg.Folder, 0o755); err != nil {
		return cfg, fmt.Errorf("cannot use watch folder %q: %w", cfg.Folder, err)
	}
	abs, err := filepath.Abs(cfg.Folder)
	if err != nil {
		return cfg, fmt.Errorf("cannot resolve watch folder: %w", err)
	}
	cfg.Folder = abs

	// Fail fast if trashing is required but no trash location is usable.
	if cfg.Force {
		log.Printf("force mode: trashing disabled, expired entries will be deleted directly")
	} else {
		desc, err := ensureTrashAvailable()
		if err != nil {
			return cfg, fmt.Errorf("no place to trash files: %w (use -force to delete instead)", err)
		}
		log.Printf("trash ready: %s", desc)
	}
	return cfg, nil
}

// syncFolder syncs the store with what is currently in the folder:
// entries that were seen before keep their original first-seen time;
// entries new to BurnBox are first seen now. Anything in the store but
// no longer present in the folder is pruned.
func syncFolder(cfg Config, st *store) {
	present := make(map[string]bool)
	if ents, err := os.ReadDir(cfg.Folder); err == nil {
		for _, de := range ents {
			name := de.Name()
			present[name] = true
			if shouldIgnore(cfg, name) {
				continue
			}
			prev, known := st.firstSeen[name]
			now := time.Now()
			if err := st.seen(name, now); err != nil {
				log.Printf("warning: cannot update store: %v", err)
			}
			if known {
				log.Printf("existing: %q (first seen %s)", name, prev.Format(time.RFC3339))
			} else {
				log.Printf("new: %q (first seen %s, expires %s)",
					name, now.Format(time.RFC3339), now.Add(cfg.Expiration).Format(time.RFC3339))
			}
		}
	} else {
		log.Printf("warning: cannot list %q: %v", cfg.Folder, err)
	}
	if err := st.prune(present); err != nil {
		log.Printf("warning: cannot prune store: %v", err)
	}
}

// runOnce implements the "once" subcommand: it performs a single scan
// of the burnbox immediately (sync the store, reap expired entries)
// and then returns without watching the folder.
func runOnce(cfg Config) error {
	cfg, err := prepareFolder(cfg)
	if err != nil {
		return err
	}
	log.Printf("scanning: %s", cfg.Folder)

	st, err := loadStore(cfg.StorePath)
	if err != nil {
		return fmt.Errorf("cannot load store: %w", err)
	}

	syncFolder(cfg, st)

	for name, firstSeen := range st.expired(cfg.Expiration, time.Now()) {
		handleExpired(cfg, st, name, firstSeen)
	}
	return nil
}

// defaultStorePath returns the default location of the first-seen store:
// <user cache dir>/BurnBox/store-<folder>-<hash>.json. The watched
// folder path is hashed so that watching several folders never clashes,
// while the readable base name keeps the cache recognizable.
func defaultStorePath(folder string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine user cache dir: %w", err)
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", fmt.Errorf("cannot resolve folder: %w", err)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(abs))
	name := fmt.Sprintf("store-%s-%016x.json", sanitizeName(filepath.Base(abs)), h.Sum64())
	return filepath.Join(cacheDir, "BurnBox", name), nil
}

// sanitizeName makes a path component safe to embed in a filename.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	if s == "" {
		return "folder"
	}
	return s
}

// isStoreFile reports whether a top-level name belongs to the first-seen
// store (the store file itself or its atomic-save temp file).
func isStoreFile(cfg Config, name string) bool {
	base := filepath.Base(cfg.StorePath)
	return name == base || name == base+".tmp"
}

// shouldIgnore reports whether a top-level name is on the ignore list.
func shouldIgnore(cfg Config, name string) bool {
	for _, ig := range cfg.Ignore {
		if name == ig {
			return true
		}
	}
	return false
}

// handleEvent reacts to a single fsnotify event on the watched folder.
func handleEvent(cfg Config, st *store, ev fsnotify.Event) {
	name := filepath.Base(ev.Name)

	// Safety: only ever consider paths that are direct children of the
	// watched folder. fsnotify on a single directory should only report
	// those, but we enforce it here regardless.
	if filepath.Dir(ev.Name) != cfg.Folder {
		log.Printf("ignoring event outside watch folder: %s", ev.Name)
		return
	}

	// The store (and its temp file during atomic saves) must never be
	// tracked or logged, otherwise saving the store generates events that
	// update the store, generating events again... forever.
	if isStoreFile(cfg, name) {
		return
	}

	switch {
	case ev.Has(fsnotify.Create):
		if shouldIgnore(cfg, name) {
			log.Printf("created (ignored): %q", name)
			return
		}
		now := time.Now()
		if err := st.seen(name, now); err != nil {
			log.Printf("warning: cannot update store: %v", err)
		}
		log.Printf("created: %q (first seen %s, expires %s)",
			name, now.Format(time.RFC3339), now.Add(cfg.Expiration).Format(time.RFC3339))

	case ev.Has(fsnotify.Remove):
		if err := st.forget(name); err != nil {
			log.Printf("warning: cannot update store: %v", err)
		}
		log.Printf("removed: %q (no longer tracked)", name)

	case ev.Has(fsnotify.Rename):
		// The old name moved away; stop tracking it. The Create event for
		// the new name is reported separately by fsnotify.
		if err := st.forget(name); err != nil {
			log.Printf("warning: cannot update store: %v", err)
		}
		log.Printf("renamed away: %q (no longer tracked)", name)
	}
}

// handleExpired trashes (or deletes) a single expired entry.
func handleExpired(cfg Config, st *store, name string, firstSeen time.Time) {
	p, err := verifiedPath(cfg, name)
	if err != nil {
		log.Printf("expired: %q rejected: %v", name, err)
		_ = st.forget(name)
		return
	}

	if _, err := os.Lstat(p); err != nil {
		// Already gone; stop tracking it.
		log.Printf("expired: %q already gone", name)
		_ = st.forget(name)
		return
	}

	if cfg.Force {
		if err := deleteInFolder(cfg, name); err != nil {
			log.Printf("expired: %q delete failed: %v", name, err)
			return
		}
		log.Printf("expired: %q deleted (force, first seen %s)", name, firstSeen.Format(time.RFC3339))
		_ = st.forget(name)
		return
	}

	if err := trashInFolder(cfg, name); err != nil {
		log.Printf("expired: %q trash failed (%v), falling back to delete", name, err)
		if err := deleteInFolder(cfg, name); err != nil {
			log.Printf("expired: %q delete failed: %v", name, err)
			return
		}
		log.Printf("expired: %q deleted (trash unavailable)", name)
		_ = st.forget(name)
		return
	}
	log.Printf("expired: %q sent to trash (first seen %s)", name, firstSeen.Format(time.RFC3339))
	_ = st.forget(name)
}

// verifiedPath joins a top-level name with the watch folder and enforces
// that the result is a direct child of the watch folder. This is the single
// choke point through which every delete/trash must pass.
func verifiedPath(cfg Config, name string) (string, error) {
	if name != filepath.Base(name) || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("not a top-level name: %q", name)
	}
	p := filepath.Clean(filepath.Join(cfg.Folder, name))
	if filepath.Dir(p) != cfg.Folder {
		return "", fmt.Errorf("path %q is not inside watch folder %q", p, cfg.Folder)
	}
	return p, nil
}

// deleteInFolder force-deletes a top-level entry inside the watch folder.
func deleteInFolder(cfg Config, name string) error {
	p, err := verifiedPath(cfg, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}
