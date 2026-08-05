package main

import (
	"context"
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
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	bolt "go.etcd.io/bbolt"
	"gopkg.in/yaml.v3"
)

const stateBucket = "spool_state"

var errHelp = errors.New("help requested")

type options struct {
	action        string
	initForce     bool
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
	FilePath string    `json:"file_path"`
	FileName string    `json:"file_name"`
	Size     int64     `json:"size"`
	MTime    time.Time `json:"mtime"`
	Inode    uint64    `json:"inode"`
	Ext      string    `json:"ext"`
}

// checkpointRecord stores only metadata. The watched file is never opened.
type checkpointRecord struct {
	Inode       uint64    `json:"inode"`
	Size        int64     `json:"size"`
	MTime       time.Time `json:"mtime"`
	StableSince time.Time `json:"stable_since"`
	Emitted     bool      `json:"emitted"`
}

type scanner struct {
	dir      string
	archive  string
	inactive time.Duration
	include  *regexp.Regexp
	exclude  *regexp.Regexp
	db       *bolt.DB
	now      func() time.Time
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
	cmd.AddCommand(initCmd)
	cmd.SetOut(stderr)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)
	flags := cmd.Flags()
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
				if err := putRecord(tx, fullPath, current); err != nil {
					return err
				}
				return nil
			}
			if previous.Emitted || now.Sub(previous.StableSince) < s.inactive {
				return nil
			}
			items = append(items, fileItem{
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
		if _, err := os.Lstat(destination); err == nil {
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
	info, err := os.Stat(o.watchDir)
	if err != nil {
		return fmt.Errorf("watch directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("watch path is not a directory: %s", o.watchDir)
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
	logger.Printf("dirwatch-cli start watch=%s scan=%s inactive=%s os=%s", o.watchDir, o.scanInterval, o.inactive, runtime.GOOS)
	s := scanner{dir: o.watchDir, archive: o.archiveDir, inactive: o.inactive, include: include, exclude: exclude, db: db, now: time.Now}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)

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
		fmt.Fprintln(os.Stderr, "dirwatch-cli:", err)
		os.Exit(1)
	}
}
