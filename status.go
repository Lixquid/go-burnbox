package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runStatus implements the "status" subcommand: it reports every entry
// in the burnbox and how long it has until deletion. It is read-only:
// it never creates the watch folder and never modifies the store.
func runStatus(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			printStatusUsage(stdout)
			return 0
		}
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg, err := parseStatusFlags(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if err := printStatus(cfg, stdout); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// printStatusUsage writes the usage information for the status
// subcommand.
func printStatusUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %s status [flags]

Reports every entry in the burnbox and how long it has until deletion,
according to the first-seen store. Does not start the daemon and does
not modify anything.

Flags:
`, filepath.Base(os.Args[0]))
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(w)
	registerStatusFlags(fs)
	fs.PrintDefaults()
}

// registerStatusFlags registers the flags shared with the start
// subcommand that are meaningful for status.
func registerStatusFlags(fs *flag.FlagSet) (folder *string, expire *time.Duration, ignore *string, storeP *string) {
	home := "."
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	} else if h, err := os.UserHomeDir(); err == nil && h != "" {
		home = h
	}

	folder = fs.String("folder", filepath.Join(home, "BurnBox"), "folder path to watch")
	expire = fs.Duration("expiration", 24*time.Hour, "expiration duration measured from when BurnBox first saw the file (e.g. 24h, 30m)")
	ignore = fs.String("ignore", ".directory", "comma-separated list of filenames to ignore")
	storeP = fs.String("store", "", "path of the first-seen store (default: <user cache dir>/BurnBox/store-<folder>.json)")
	return
}

// parseStatusFlags builds the Config from a flag set and argument list
// for the status subcommand. It mirrors parseFlags, minus the force
// flag, which has no meaning for status.
func parseStatusFlags(fs *flag.FlagSet, args []string) (Config, error) {
	folder, expire, ignore, storeP := registerStatusFlags(fs)
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
		// Same default as the start subcommand, keyed by the watched
		// folder path.
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
		StorePath:  storePath,
	}, nil
}

// statusRow is one line of status output.
type statusRow struct {
	name      string
	firstSeen time.Time
	remaining time.Duration // time until deletion; negative once expired
	tracked   bool          // known to the first-seen store
}

// printStatus renders the status report for the configured burnbox.
func printStatus(cfg Config, w io.Writer) error {
	abs, err := filepath.Abs(cfg.Folder)
	if err != nil {
		return fmt.Errorf("cannot resolve folder: %w", err)
	}

	ents, err := os.ReadDir(abs)
	if err != nil {
		return fmt.Errorf("cannot list %q: %w", abs, err)
	}

	st, err := loadStore(cfg.StorePath)
	if err != nil {
		return fmt.Errorf("cannot load store: %w", err)
	}
	all := st.peekAll()

	now := time.Now()
	var rows []statusRow
	present := make(map[string]bool, len(ents))
	for _, de := range ents {
		name := de.Name()
		if shouldIgnore(cfg, name) || isStoreFile(cfg, name) {
			continue
		}
		present[name] = true
		firstSeen, tracked := all[name]
		if !tracked {
			// Not yet tracked: the daemon has not recorded a first-seen
			// time for it (it appeared while BurnBox was not running).
			rows = append(rows, statusRow{name: name})
			continue
		}
		rows = append(rows, statusRow{
			name:      name,
			firstSeen: firstSeen,
			remaining: firstSeen.Add(cfg.Expiration).Sub(now),
			tracked:   true,
		})
	}

	// Soonest deletion first; untracked names last, alphabetically.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].tracked != rows[j].tracked {
			return rows[i].tracked
		}
		if rows[i].tracked {
			return rows[i].remaining < rows[j].remaining
		}
		return rows[i].name < rows[j].name
	})

	fmt.Fprintf(w, "BurnBox: %s\n", abs)
	fmt.Fprintf(w, "Entries expire %s after first seen (expiration %s)\n\n", cfg.Expiration, cfg.Expiration)

	if len(rows) == 0 {
		fmt.Fprintln(w, "No files in the burnbox.")
	}

	nameWidth := len("NAME")
	for _, r := range rows {
		if n := len(r.name); n > nameWidth {
			nameWidth = n
		}
	}
	fmt.Fprintf(w, "%-*s  %-16s %s\n", nameWidth, "NAME", "DELETES IN", "FIRST SEEN")
	for _, r := range rows {
		if !r.tracked {
			fmt.Fprintf(w, "%-*s  %-16s %s\n", nameWidth, r.name, "untracked", "-")
			continue
		}
		fmt.Fprintf(w, "%-*s  %-16s %s\n",
			nameWidth, r.name, formatRemaining(r.remaining), r.firstSeen.Format(time.RFC3339))
	}

	// Entries still in the store but missing from the folder: the daemon
	// prunes these on its next start.
	var gone []string
	for name := range all {
		if !present[name] && !shouldIgnore(cfg, name) {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		fmt.Fprintf(w, "\nIn store but no longer in the folder (pruned on next start):\n")
		for _, name := range gone {
			fmt.Fprintf(w, "  %s\n", name)
		}
	}

	return nil
}

// formatRemaining renders a duration until deletion in a compact,
// human-readable form, e.g. "23h59m1s" or "2d3h".
func formatRemaining(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	d = d.Round(time.Second)
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, "")
}
