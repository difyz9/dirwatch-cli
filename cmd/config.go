package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type options struct {
	action, eventID, stateStatus, execCommand, retryReason string
	initForce                                              bool
	configPath, watchDir, archiveDir                       string
	checkpoint, includeSource, excludeSource               string
	scanInterval, inactive, lease, waitTimeout, retryDelay time.Duration
	maxFiles, maxInflight, maxAttempts                     int
}

type yamlConfig struct {
	Watch        *string `yaml:"watch"`
	ArchiveDir   *string `yaml:"archive_dir"`
	ScanInterval *string `yaml:"scan_interval"`
	Inactive     *string `yaml:"inactive"`
	Checkpoint   *string `yaml:"checkpoint"`
	Include      *string `yaml:"include"`
	Exclude      *string `yaml:"exclude"`
	Queue        *struct {
		MaxInflight *int    `yaml:"max_inflight"`
		RetryDelay  *string `yaml:"retry_delay"`
		MaxAttempts *int    `yaml:"max_attempts"`
	} `yaml:"queue"`
}

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "dirwatch-cli", "dirwatch.yaml"), err
}

func defaultStatePath() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		return filepath.Join(stateHome, "dirwatch-cli", "state.db"), nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "dirwatch-cli", "state.db"), err
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

func loadYAML(path string, required bool, o *options) error {
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
	if cfg.Queue != nil {
		if cfg.Queue.MaxInflight != nil {
			o.maxInflight = *cfg.Queue.MaxInflight
		}
		if cfg.Queue.MaxAttempts != nil {
			o.maxAttempts = *cfg.Queue.MaxAttempts
		}
		if cfg.Queue.RetryDelay != nil {
			o.retryDelay, err = time.ParseDuration(*cfg.Queue.RetryDelay)
			if err != nil {
				return fmt.Errorf("config queue.retry_delay: %w", err)
			}
		}
	}
	return nil
}

const defaultConfig = `# dirwatch-cli configuration
watch: /data/incoming
archive_dir: ""
scan_interval: 2s
inactive: 3s
include: '\.csv$|\.jpg$|\.mp4$'
exclude: '\.tmp$|\.part$'
queue:
  max_inflight: 1
  retry_delay: 30s
  max_attempts: 5
`

func writeInitialConfig(path string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
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
		return err
	}
	return file.Close()
}
