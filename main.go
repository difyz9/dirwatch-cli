package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	bolt "go.etcd.io/bbolt"
	"gopkg.in/yaml.v3"
)

const stateBucket = "spool_state"

var (
	errHelp        = errors.New("help requested")
	errWaitTimeout = errors.New("wait timeout")
)

type options struct {
	action        string
	initForce     bool
	lease         time.Duration
	waitTimeout   time.Duration
	maxFiles      int
	eventID       string
	stateStatus   string
	configPath    string
	watchDir      string
	archiveDir    string
	scanInterval  time.Duration
	inactive      time.Duration
	checkpoint    string
	includeSource string
	excludeSource string
}

type yamlConfig struct {
	Watch        *string `yaml:"watch"`
	ArchiveDir   *string `yaml:"archive_dir"`
	ScanInterval *string `yaml:"scan_interval"`
	Inactive     *string `yaml:"inactive"`
	Checkpoint   *string `yaml:"checkpoint"`
	Include      *string `yaml:"include"`
	Exclude      *string `yaml:"exclude"`
}

type fileItem struct {
	EventID  string     `json:"event_id"`
	Event    string     `json:"event"`
	Status   string     `json:"status"`
	LeaseEnd *time.Time `json:"lease_until,omitempty"`
	FilePath string     `json:"file_path"`
	FileName string     `json:"file_name"`
	Size     int64      `json:"size"`
	MTime    time.Time  `json:"mtime"`
	Inode    uint64     `json:"inode"`
	Ext      string     `json:"ext"`
}

// checkpointRecord stores only metadata. The watched file is never opened.
type checkpointRecord struct {
	Inode       uint64    `json:"inode"`
	Size        int64     `json:"size"`
	MTime       time.Time `json:"mtime"`
	StableSince time.Time `json:"stable_since"`
	Emitted     bool      `json:"emitted"`
	EventID     string    `json:"event_id,omitempty"`
	Status      string    `json:"status,omitempty"`
	LeaseUntil  time.Time `json:"lease_until,omitempty"`
	AckedAt     time.Time `json:"acknowledged_at,omitempty"`
}

type scanner struct {
	dir      string
	archive  string
	inactive time.Duration
	include  *regexp.Regexp
	exclude  *regexp.Regexp
	db       *bolt.DB
	now      func() time.Time
	lease    time.Duration
}

func newEventID(now time.Time) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return strconv.FormatInt(now.UnixMilli(), 36) + "-" + fmt.Sprintf("%x", random[:]), nil
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "dirwatch-cli", "dirwatch.yaml"), nil
}

func configArgument(args []string, fallback string) (string, bool, error) {
	for i, arg := range args {
		if arg == "--config" {
			if i+1 >= len(args) {
				return "", true, errors.New("--config requires a path")
			}
			return args[i+1], true, nil
		}
		if strings.HasPrefix(arg, "--config=") {
			return strings.SplitN(arg, "=", 2)[1], true, nil
		}
	}
	return fallback, false, nil
}

func loadYAMLConfig(path string, required bool, o *options) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil
		}
		return fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg yamlConfig
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}
	if cfg.Watch != nil {
		o.watchDir = *cfg.Watch
	}
	if cfg.ArchiveDir != nil {
		o.archiveDir = *cfg.ArchiveDir
	}
	if cfg.Checkpoint != nil {
		o.checkpoint = *cfg.Checkpoint
	}
	if cfg.Include != nil {
		o.includeSource = *cfg.Include
	}
	if cfg.Exclude != nil {
		o.excludeSource = *cfg.Exclude
	}
	if cfg.ScanInterval != nil {
		o.scanInterval, err = time.ParseDuration(*cfg.ScanInterval)
		if err != nil {
			return fmt.Errorf("config scan_interval: %w", err)
		}
	}
	if cfg.Inactive != nil {
		o.inactive, err = time.ParseDuration(*cfg.Inactive)
		if err != nil {
			return fmt.Errorf("config inactive: %w", err)
		}
	}
	return nil
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	defaultPath, err := defaultConfigPath()
	if err != nil {
		return options{}, fmt.Errorf("resolve user home: %w", err)
	}
	o := options{
		watchDir: "/data/incoming", scanInterval: 2 * time.Second, inactive: 3 * time.Second,
		checkpoint: "./dirwatch.db", includeSource: `\.csv$|\.jpg$|\.mp4$`, excludeSource: `\.tmp$|\.part$`,
		lease: 5 * time.Minute, maxFiles: 1,
	}
	configPath, explicitConfig, err := configArgument(args, defaultPath)
	if err != nil {
		return o, err
	}
	o.configPath = configPath
	isInit := len(args) > 0 && args[0] == "init"
	if !isInit {
		if err := loadYAMLConfig(configPath, explicitConfig, &o); err != nil {
			return o, err
		}
	}

	executed := false
	cmd := &cobra.Command{
		Use:           "dirwatch-cli",
		Short:         "Watch directories and emit metadata for completed files",
		Version:       "dev",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		Example: `  dirwatch-cli
  dirwatch-cli --watch /data/incoming --archive-dir /data/archive
  dirwatch-cli --config ~/.config/dirwatch-cli/dirwatch.yaml`,
		RunE: func(cmd *cobra.Command, positional []string) error {
			executed = true
			o.action = "watch"
			if o.scanInterval <= 0 {
				return errors.New("--scan-interval must be greater than zero")
			}
			if o.inactive < 0 {
				return errors.New("--inactive cannot be negative")
			}
			if o.watchDir == "" || o.checkpoint == "" {
				return errors.New("--watch and --checkpoint cannot be empty")
			}
			return nil
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	initCmd := &cobra.Command{
		Use:           "init",
		Short:         "Create the default YAML configuration",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, positional []string) {
			executed = true
			o.action = "init"
		},
	}
	initCmd.Flags().BoolVarP(&o.initForce, "force", "f", false, "overwrite an existing configuration")
	waitCmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait for ready files, claim them, then exit",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, positional []string) {
			executed = true
			o.action = "wait"
		},
	}
	waitCmd.Flags().DurationVar(&o.lease, "lease", o.lease, "claim lease before an unacknowledged event can be redelivered")
	waitCmd.Flags().DurationVar(&o.waitTimeout, "timeout", 0, "maximum wait time; zero waits indefinitely")
	waitCmd.Flags().IntVar(&o.maxFiles, "max-files", o.maxFiles, "maximum number of files to claim")
	ackCmd := &cobra.Command{
		Use:   "ack EVENT_ID",
		Short: "Acknowledge a claimed event and archive its file",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, positional []string) {
			executed = true
			o.action, o.eventID = "ack", positional[0]
		},
	}
	nackCmd := &cobra.Command{
		Use:   "nack EVENT_ID",
		Short: "Release a claimed event for immediate redelivery",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, positional []string) {
			executed = true
			o.action, o.eventID = "nack", positional[0]
		},
	}
	stateCmd := &cobra.Command{Use: "state", Short: "Inspect persistent delivery state"}
	stateListCmd := &cobra.Command{
		Use:   "list",
		Short: "List checkpoint records as JSON",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, positional []string) {
			executed = true
			o.action = "state-list"
		},
	}
	stateListCmd.Flags().StringVar(&o.stateStatus, "status", "", "filter by observing, ready, claimed, or acknowledged")
	stateCmd.AddCommand(stateListCmd)
	cmd.AddCommand(initCmd, waitCmd, ackCmd, nackCmd, stateCmd)
	cmd.SetOut(stderr)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)
	flags := cmd.PersistentFlags()
	flags.StringVar(&o.configPath, "config", o.configPath, "YAML config path")
	flags.StringVar(&o.watchDir, "watch", o.watchDir, "directory to monitor")
	flags.StringVar(&o.archiveDir, "archive-dir", o.archiveDir, "move emitted files here, preserving subdirectories")
	flags.DurationVar(&o.scanInterval, "scan-interval", o.scanInterval, "polling interval")
	flags.DurationVar(&o.inactive, "inactive", o.inactive, "required unchanged duration")
	flags.StringVar(&o.checkpoint, "checkpoint", o.checkpoint, "bbolt checkpoint path")
	flags.StringVar(&o.includeSource, "include", o.includeSource, "include regular expression; empty includes all")
	flags.StringVar(&o.excludeSource, "exclude", o.excludeSource, "exclude regular expression; empty disables it")
	if err := cmd.Execute(); err != nil {
		return o, err
	}
	if !executed {
		return o, errHelp
	}
	if o.action != "init" {
		if o.scanInterval <= 0 || o.inactive < 0 || o.watchDir == "" || o.checkpoint == "" {
			return o, errors.New("invalid watch, checkpoint, scan interval, or inactive setting")
		}
		if o.action == "wait" && (o.lease <= 0 || o.waitTimeout < 0 || o.maxFiles <= 0) {
			return o, errors.New("--lease and --max-files must be positive; --timeout cannot be negative")
		}
		if o.action == "state-list" && o.stateStatus != "" && o.stateStatus != "observing" && o.stateStatus != "ready" && o.stateStatus != "claimed" && o.stateStatus != "acknowledged" {
			return o, errors.New("--status must be observing, ready, claimed, or acknowledged")
		}
	}
	return o, nil
}

const defaultConfig = `# dirwatch-cli configuration
watch: /data/incoming
archive_dir: ""
scan_interval: 2s
inactive: 3s
checkpoint: ./dirwatch.db
include: '\.csv$|\.jpg$|\.mp4$'
exclude: '\.tmp$|\.part$'
`

func initConfig(path string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("config already exists: %s (use --force to overwrite)", path)
		}
		return fmt.Errorf("create config %q: %w", path, err)
	}
	if _, err := io.WriteString(file, defaultConfig); err != nil {
		file.Close()
		return fmt.Errorf("write config %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config %q: %w", path, err)
	}
	return nil
}

func compileOptional(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile(pattern)
}

func openCheckpoint(path string) (*bolt.DB, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(stateBucket))
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}

func sameVersion(a, b checkpointRecord) bool {
	return a.Inode == b.Inode && a.Size == b.Size && a.MTime.Equal(b.MTime)
}

func (s *scanner) matches(name string) bool {
	return (s.include == nil || s.include.MatchString(name)) &&
		(s.exclude == nil || !s.exclude.MatchString(name))
}

func getRecord(tx *bolt.Tx, path string) (checkpointRecord, bool, error) {
	raw := tx.Bucket([]byte(stateBucket)).Get([]byte(path))
	if raw == nil {
		return checkpointRecord{}, false, nil
	}
	var record checkpointRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, false, fmt.Errorf("decode checkpoint for %q: %w", path, err)
	}
	return record, true, nil
}

func putRecord(tx *bolt.Tx, path string, record checkpointRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(stateBucket)).Put([]byte(path), raw)
}

func isWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// scan recursively performs metadata-only stat calls; it never opens a watched file.
func (s *scanner) scan() ([]fileItem, error) {
	now := s.now()
	seen := make(map[string]struct{})
	items := make([]fileItem, 0)

	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(stateBucket))
		walkErr := filepath.WalkDir(s.dir, func(fullPath string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				// Files may disappear between directory reads on an active spool.
				if errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() {
				if fullPath != s.dir && s.archive != "" && isWithin(fullPath, s.archive) {
					return filepath.SkipDir
				}
				return nil
			}
			if !s.matches(entry.Name()) {
				return nil
			}
			info, statErr := entry.Info()
			if statErr != nil || !info.Mode().IsRegular() {
				return nil
			}
			seen[fullPath] = struct{}{}
			current := checkpointRecord{Inode: inodeOf(info), Size: info.Size(), MTime: info.ModTime()}
			previous, found, getErr := getRecord(tx, fullPath)
			if getErr != nil {
				return getErr
			}
			if !found || !sameVersion(current, previous) {
				current.StableSince = now
				current.Status = "observing"
				if err := putRecord(tx, fullPath, current); err != nil {
					return err
				}
				return nil
			}
			if previous.Emitted {
				// Old records without a status were emitted by pre-lease versions.
				if previous.Status != "claimed" || now.Before(previous.LeaseUntil) {
					return nil
				}
			} else if now.Sub(previous.StableSince) < s.inactive {
				return nil
			}
			if previous.EventID == "" {
				previous.EventID, getErr = newEventID(now)
				if getErr != nil {
					return getErr
				}
			}
			if s.lease > 0 {
				previous.Status = "claimed"
				previous.LeaseUntil = now.Add(s.lease)
			} else {
				previous.Status = "acknowledged"
				previous.AckedAt = now
			}
			items = append(items, fileItem{
				EventID: previous.EventID, Event: "file_ready", Status: previous.Status, LeaseEnd: optionalTime(previous.LeaseUntil),
				FilePath: fullPath, FileName: entry.Name(), Size: info.Size(),
				MTime: info.ModTime(), Inode: current.Inode, Ext: filepath.Ext(entry.Name()),
			})
			previous.Emitted = true
			if err := putRecord(tx, fullPath, previous); err != nil {
				return err
			}
			return nil
		})
		if walkErr != nil {
			return walkErr
		}

		// cleanup_removed: discard checkpoints for paths no longer present or filtered out.
		cursor := bucket.Cursor()
		for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
			if _, ok := seen[string(key)]; !ok {
				record, _, recordErr := getRecord(tx, string(key))
				if recordErr != nil {
					return recordErr
				}
				if record.Status == "claimed" {
					continue
				}
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	sort.Slice(items, func(i, j int) bool { return items[i].FilePath < items[j].FilePath })
	return items, err
}

func (s *scanner) archiveItems(items []fileItem) []error {
	if s.archive == "" {
		return nil
	}
	var errs []error
	for _, item := range items {
		rel, err := filepath.Rel(s.dir, item.FilePath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			errs = append(errs, fmt.Errorf("archive %q: path escapes watch directory", item.FilePath))
			continue
		}
		destination := filepath.Join(s.archive, rel)
		if destinationInfo, err := os.Lstat(destination); err == nil {
			_, sourceErr := os.Lstat(item.FilePath)
			if errors.Is(sourceErr, os.ErrNotExist) && destinationInfo.Mode().IsRegular() &&
				destinationInfo.Size() == item.Size && destinationInfo.ModTime().Equal(item.MTime) &&
				(item.Inode == 0 || inodeOf(destinationInfo) == item.Inode) {
				// A previous ack moved the same file but stopped before reporting success.
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

type stateItem struct {
	FilePath string `json:"file_path"`
	checkpointRecord
}

func recordItem(path string, record checkpointRecord) fileItem {
	return fileItem{
		EventID: record.EventID, Event: "file_ready", Status: record.Status, LeaseEnd: optionalTime(record.LeaseUntil),
		FilePath: path, FileName: filepath.Base(path), Size: record.Size, MTime: record.MTime,
		Inode: record.Inode, Ext: filepath.Ext(path),
	}
}

func updateEvent(db *bolt.DB, eventID, action string, now time.Time) (fileItem, error) {
	var item fileItem
	found := false
	err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(stateBucket))
		return bucket.ForEach(func(key, value []byte) error {
			if found {
				return nil
			}
			var record checkpointRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.EventID != eventID {
				return nil
			}
			found = true
			switch action {
			case "ack":
				if record.Status != "claimed" && record.Status != "acknowledged" {
					return fmt.Errorf("event %s is %s, not claimed", eventID, record.Status)
				}
				record.Status, record.AckedAt, record.LeaseUntil = "acknowledged", now, time.Time{}
			case "nack":
				if record.Status != "claimed" {
					return fmt.Errorf("event %s is %s, not claimed", eventID, record.Status)
				}
				record.Emitted, record.Status, record.LeaseUntil = false, "ready", time.Time{}
			default:
				return fmt.Errorf("unknown event action %q", action)
			}
			item = recordItem(string(key), record)
			return putRecord(tx, string(key), record)
		})
	})
	if err != nil {
		return item, err
	}
	if !found {
		return item, fmt.Errorf("event not found: %s", eventID)
	}
	return item, nil
}

func listState(db *bolt.DB, status string) ([]stateItem, error) {
	items := make([]stateItem, 0)
	err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(stateBucket)).ForEach(func(key, value []byte) error {
			var record checkpointRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			effective := record.Status
			if effective == "" {
				if record.Emitted {
					effective = "acknowledged"
				} else {
					effective = "observing"
				}
			}
			if status != "" && effective != status {
				return nil
			}
			record.Status = effective
			items = append(items, stateItem{FilePath: string(key), checkpointRecord: record})
			return nil
		})
	})
	sort.Slice(items, func(i, j int) bool { return items[i].FilePath < items[j].FilePath })
	return items, err
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	if o.action == "init" {
		if err := initConfig(o.configPath, o.initForce); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created %s\n", o.configPath)
		return nil
	}
	include, err := compileOptional(o.includeSource)
	if err != nil {
		return fmt.Errorf("invalid --include regex: %w", err)
	}
	exclude, err := compileOptional(o.excludeSource)
	if err != nil {
		return fmt.Errorf("invalid --exclude regex: %w", err)
	}
	if o.action == "watch" || o.action == "wait" {
		info, statErr := os.Stat(o.watchDir)
		if statErr != nil {
			return fmt.Errorf("watch directory: %w", statErr)
		}
		if !info.IsDir() {
			return fmt.Errorf("watch path is not a directory: %s", o.watchDir)
		}
	}
	watchAbs, err := filepath.Abs(o.watchDir)
	if err != nil {
		return fmt.Errorf("resolve watch directory: %w", err)
	}
	o.watchDir = filepath.Clean(watchAbs)
	if o.archiveDir != "" {
		archiveAbs, absErr := filepath.Abs(o.archiveDir)
		if absErr != nil {
			return fmt.Errorf("resolve archive directory: %w", absErr)
		}
		o.archiveDir = filepath.Clean(archiveAbs)
		if o.archiveDir == o.watchDir {
			return errors.New("--archive-dir must not equal --watch")
		}
	}
	db, err := openCheckpoint(o.checkpoint)
	if err != nil {
		return fmt.Errorf("open checkpoint: %w", err)
	}
	defer db.Close()

	logger := log.New(stderr, "", log.LstdFlags)
	s := scanner{dir: o.watchDir, archive: o.archiveDir, inactive: o.inactive, include: include, exclude: exclude, db: db, now: time.Now}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)

	switch o.action {
	case "ack", "nack":
		item, updateErr := updateEvent(db, o.eventID, o.action, time.Now())
		if updateErr != nil {
			return updateErr
		}
		if o.action == "ack" {
			if archiveErrs := s.archiveItems([]fileItem{item}); len(archiveErrs) > 0 {
				return archiveErrs[0]
			}
		}
		return encoder.Encode(item)
	case "state-list":
		items, listErr := listState(db, o.stateStatus)
		if listErr != nil {
			return listErr
		}
		return encoder.Encode(items)
	case "wait":
		s.lease = o.lease
		var timeout <-chan time.Time
		var timer *time.Timer
		if o.waitTimeout > 0 {
			timer = time.NewTimer(o.waitTimeout)
			defer timer.Stop()
			timeout = timer.C
		}
		for {
			items, scanErr := s.scan()
			if scanErr != nil {
				return scanErr
			}
			if len(items) > o.maxFiles {
				// scan claims in path order; release claims beyond the requested batch.
				for _, extra := range items[o.maxFiles:] {
					_, _ = updateEvent(db, extra.EventID, "nack", time.Now())
				}
				items = items[:o.maxFiles]
			}
			if len(items) > 0 {
				for _, item := range items {
					if err := encoder.Encode(item); err != nil {
						return err
					}
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timeout:
				return errWaitTimeout
			case <-time.After(o.scanInterval):
			}
		}
	}

	logger.Printf("dirwatch-cli start watch=%s scan=%s inactive=%s os=%s", o.watchDir, o.scanInterval, o.inactive, runtime.GOOS)

	// Scan immediately, then poll. Polling is authoritative and works on NFS.
	scan := func() {
		items, scanErr := s.scan()
		if scanErr != nil {
			logger.Printf("scan error: %v", scanErr)
			return
		}
		if len(items) > 0 {
			if encodeErr := encoder.Encode(items); encodeErr != nil {
				logger.Printf("stdout encode error: %v", encodeErr)
				return
			}
			for _, archiveErr := range s.archiveItems(items) {
				logger.Printf("%v", archiveErr)
			}
		}
	}
	scan()
	ticker := time.NewTicker(o.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Print("dirwatch-cli stopped")
			return nil
		case <-ticker.C:
			scan()
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errHelp) {
			return
		}
		if errors.Is(err, errWaitTimeout) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "dirwatch-cli:", err)
		os.Exit(1)
	}
}
