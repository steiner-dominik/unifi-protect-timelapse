// Package archive provides read-only browsing of stored frames. It reads from
// the archive directory and the spool directory and merges the two, so images
// that have not been synced yet are still visible.
//
// Every path component supplied by a client is validated against a strict
// pattern and the resolved path is checked to be inside its root, so no request
// can escape the configured directories.
package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

var (
	yearPattern  = regexp.MustCompile(`^\d{4}$`)
	monthPattern = regexp.MustCompile(`^\d{4}-\d{2}$`)
	dayPattern   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// Frame names are restricted to what this service produces, plus whatever
	// the Raspberry Pi wrote historically: a prefix, a timestamp and .jpg.
	framePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.[jJ][pP][eE]?[gG]$`)
)

// ErrInvalidName reports a path component that failed validation.
var ErrInvalidName = errors.New("invalid name")

// ErrNotFound reports that no matching frame exists in any root.
var ErrNotFound = errors.New("not found")

// Browser lists frames across the archive and spool directories.
type Browser struct {
	cfg *config.Config
}

// New returns a Browser.
func New(cfg *config.Config) *Browser { return &Browser{cfg: cfg} }

// roots returns the directories to search, archive first so that synced frames
// take precedence over any spool leftovers.
func (b *Browser) roots() []string {
	var roots []string
	if b.cfg.SyncEnabled() && b.cfg.Sync.ArchiveDir != "" {
		roots = append(roots, b.cfg.Sync.ArchiveDir)
	}
	roots = append(roots, b.cfg.Capture.SpoolDir)
	return roots
}

// Years returns every year that contains frames, newest first.
func (b *Browser) Years() []string {
	return b.listMerged("", yearPattern, true)
}

// Months returns the months within a year, newest first.
func (b *Browser) Months(year string) ([]string, error) {
	if !yearPattern.MatchString(year) {
		return nil, fmt.Errorf("%w: year %q", ErrInvalidName, year)
	}
	return b.listMerged(year, monthPattern, true), nil
}

// Days returns the days within a month, newest first.
func (b *Browser) Days(month string) ([]string, error) {
	if !monthPattern.MatchString(month) {
		return nil, fmt.Errorf("%w: month %q", ErrInvalidName, month)
	}
	year, _, _ := strings.Cut(month, "-")
	return b.listMerged(filepath.Join(year, month), dayPattern, true), nil
}

// Frame is one stored image.
type Frame struct {
	Name  string `json:"name"`
	Time  string `json:"time"`
	Bytes int64  `json:"bytes"`
	// Group is the filename prefix, which distinguishes image series captured
	// by different sources over the years.
	Group string `json:"group"`
}

// Frames returns the frames of a day in chronological order.
func (b *Browser) Frames(day string) ([]Frame, error) {
	dir, err := relDirForDay(day)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var frames []Frame

	for _, root := range b.roots() {
		full, err := safeJoin(root, dir)
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(full)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !framePattern.MatchString(name) {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}

			var size int64
			if info, err := entry.Info(); err == nil {
				size = info.Size()
			}
			frames = append(frames, Frame{
				Name:  name,
				Time:  timeFromName(name, day),
				Bytes: size,
				Group: groupFromName(name),
			})
		}
	}

	sort.Slice(frames, func(i, j int) bool { return frames[i].Name < frames[j].Name })
	return frames, nil
}

// FramePath resolves a frame to a file on disk, searching every root.
func (b *Browser) FramePath(day, name string) (string, error) {
	dir, err := relDirForDay(day)
	if err != nil {
		return "", err
	}
	if !framePattern.MatchString(name) {
		return "", fmt.Errorf("%w: frame %q", ErrInvalidName, name)
	}

	for _, root := range b.roots() {
		path, err := safeJoin(root, filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", ErrNotFound
}

// listMerged returns the entries of relDir under every root whose names match
// pattern, deduplicated.
func (b *Browser) listMerged(relDir string, pattern *regexp.Regexp, descending bool) []string {
	seen := make(map[string]struct{})
	var names []string

	for _, root := range b.roots() {
		dir := root
		if relDir != "" {
			joined, err := safeJoin(root, relDir)
			if err != nil {
				continue
			}
			dir = joined
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !pattern.MatchString(name) {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}

	slices.Sort(names)
	if descending {
		slices.Reverse(names)
	}
	return names
}

// relDirForDay converts "2026-09-07" into "2026/2026-09/2026-09-07".
func relDirForDay(day string) (string, error) {
	if !dayPattern.MatchString(day) {
		return "", fmt.Errorf("%w: day %q", ErrInvalidName, day)
	}
	year := day[:4]
	month := day[:7]
	return filepath.Join(year, month, day), nil
}

// safeJoin joins rel onto root and verifies the result stays inside root, so a
// crafted component can never escape the configured directory even if the
// pattern checks were somehow bypassed.
func safeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute path", ErrInvalidName)
	}
	joined := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes root", ErrInvalidName)
	}
	return joined, nil
}

// timeFromName extracts "HH:MM:SS" from a frame name that ends with the
// standard timestamp, falling back to an empty string for foreign names.
func timeFromName(name, day string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(name, filepath.Ext(name)), ".")
	idx := strings.Index(base, day)
	if idx < 0 {
		return ""
	}
	rest := base[idx+len(day):]
	// rest is "-HH-MM-SS".
	if len(rest) != 9 || rest[0] != '-' {
		return ""
	}
	return rest[1:3] + ":" + rest[4:6] + ":" + rest[7:9]
}

// groupFromName returns the filename prefix that precedes the timestamp.
func groupFromName(name string) string {
	idx := strings.Index(name, "-")
	if idx < 4 {
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	// Walk back to the separator before the four-digit year.
	prefixEnd := idx - 4
	if prefixEnd < 0 || prefixEnd > len(name) {
		return ""
	}
	return name[:prefixEnd]
}
