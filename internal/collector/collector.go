package collector

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dirwatch-cli/internal/checkpoint"
	"dirwatch-cli/internal/model"
)

type Options struct {
	WatchDir    string
	ArchiveDir  string
	Inactive    time.Duration
	Include     *regexp.Regexp
	Exclude     *regexp.Regexp
	Lease       time.Duration
	MaxItems    int
	MaxInflight int
	MaxAttempts int
}

type Collector struct {
	options Options
	store   *checkpoint.Store
	now     func() time.Time
}

func New(options Options, store *checkpoint.Store) *Collector {
	return &Collector{options: options, store: store, now: time.Now}
}

func (c *Collector) SetClock(now func() time.Time) { c.now = now }

func sameVersion(a, b model.CheckpointRecord) bool {
	return a.Inode == b.Inode && a.Size == b.Size && a.MTime.Equal(b.MTime)
}

func (c *Collector) matches(name string) bool {
	return (c.options.Include == nil || c.options.Include.MatchString(name)) &&
		(c.options.Exclude == nil || !c.options.Exclude.MatchString(name))
}

func isWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func newEventID(now time.Time) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return strconv.FormatInt(now.UnixMilli(), 36) + "-" + fmt.Sprintf("%x", random[:]), nil
}

// Scan recursively stats files, updates stability, and atomically claims ready records.
// Watched file contents are never opened.
func (c *Collector) Scan() ([]model.FileItem, error) {
	now := c.now()
	seen := make(map[string]struct{})
	err := filepath.WalkDir(c.options.WatchDir, func(fullPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			if fullPath != c.options.WatchDir && c.options.ArchiveDir != "" && isWithin(fullPath, c.options.ArchiveDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !c.matches(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		seen[fullPath] = struct{}{}
		current := model.CheckpointRecord{Inode: inodeOf(info), Size: info.Size(), MTime: info.ModTime()}
		previous, found, err := c.store.Get(fullPath)
		if err != nil {
			return err
		}
		if !found || !sameVersion(current, previous) {
			current.StableSince, current.FirstSeenAt, current.Status = now, now, model.StatusObserving
			return c.store.Put(fullPath, current)
		}
		if effectiveRecordStatus(previous) != model.StatusObserving || now.Sub(previous.StableSince) < c.options.Inactive {
			return nil
		}
		if previous.EventID == "" {
			previous.EventID, err = newEventID(now)
			if err != nil {
				return err
			}
		}
		previous.Status, previous.ReadyAt, previous.Emitted = model.StatusReady, now, false
		return c.store.Put(fullPath, previous)
	})
	if err != nil {
		return nil, err
	}
	states, err := c.store.List("")
	if err != nil {
		return nil, err
	}
	for _, state := range states {
		if _, ok := seen[state.FilePath]; ok || state.Status == model.StatusClaimed {
			continue
		}
		if err := c.store.Delete(state.FilePath); err != nil {
			return nil, err
		}
	}
	limit := c.options.MaxItems
	if limit <= 0 {
		limit = int(^uint(0) >> 1)
	}
	items, err := c.store.ClaimReady(now, checkpoint.QueueOptions{Limit: limit, MaxInflight: c.options.MaxInflight,
		Lease: c.options.Lease, MaxAttempts: c.options.MaxAttempts})
	if err != nil {
		return nil, err
	}
	if c.options.Lease > 0 {
		return items, nil
	}
	for i := range items {
		acked, err := c.store.UpdateEvent(items[i].EventID, "ack", now, 0, 0, "")
		if err != nil {
			return nil, err
		}
		items[i] = acked
	}
	return items, nil
}

func effectiveRecordStatus(record model.CheckpointRecord) string {
	if record.Status != "" {
		return record.Status
	}
	if record.Emitted {
		return model.StatusAcknowledged
	}
	return model.StatusObserving
}

func (c *Collector) Archive(items []model.FileItem) []error {
	if c.options.ArchiveDir == "" {
		return nil
	}
	var errs []error
	for _, item := range items {
		rel, err := filepath.Rel(c.options.WatchDir, item.FilePath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			errs = append(errs, fmt.Errorf("archive %q: path escapes watch directory", item.FilePath))
			continue
		}
		destination := filepath.Join(c.options.ArchiveDir, rel)
		if destinationInfo, err := os.Lstat(destination); err == nil {
			_, sourceErr := os.Lstat(item.FilePath)
			if errors.Is(sourceErr, os.ErrNotExist) && destinationInfo.Mode().IsRegular() &&
				destinationInfo.Size() == item.Size && destinationInfo.ModTime().Equal(item.MTime) &&
				(item.Inode == 0 || inodeOf(destinationInfo) == item.Inode) {
				continue
			}
			errs = append(errs, fmt.Errorf("archive %q: destination already exists: %s", item.FilePath, destination))
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("archive %q: inspect destination: %w", item.FilePath, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("archive %q: create directory: %w", item.FilePath, err))
			continue
		}
		if err := os.Rename(item.FilePath, destination); err != nil {
			errs = append(errs, fmt.Errorf("archive %q to %q: %w", item.FilePath, destination, err))
		}
	}
	return errs
}
