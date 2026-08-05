package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"dirwatch-cli/internal/checkpoint"
	"dirwatch-cli/internal/collector"
	"dirwatch-cli/internal/model"
	"github.com/spf13/cobra"
)

var (
	errWaitTimeout = errors.New("wait timeout")
	version        = "dev" // Replaced with -ldflags for tagged releases.
)

func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errWaitTimeout) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "dirwatch-cli:", err)
		os.Exit(1)
	}
}

func initialOptions(args []string) (options, error) {
	configPath, err := defaultConfigPath()
	if err != nil {
		return options{}, err
	}
	statePath, err := defaultStatePath()
	if err != nil {
		return options{}, err
	}
	o := options{watchDir: "/data/incoming", scanInterval: 2 * time.Second, inactive: 3 * time.Second,
		checkpoint: statePath, includeSource: `\.csv$|\.jpg$|\.mp4$`, excludeSource: `\.tmp$|\.part$`,
		lease: 5 * time.Minute, maxFiles: 1}
	var explicit bool
	o.configPath, explicit, err = configArgument(args, configPath)
	if err != nil {
		return o, err
	}
	if !(len(args) > 0 && args[0] == "init") {
		if err := loadYAML(o.configPath, explicit, &o); err != nil {
			return o, err
		}
	}
	return o, nil
}

func newRoot(args []string, stderr io.Writer) (*cobra.Command, *options, error) {
	loaded, err := initialOptions(args)
	if err != nil {
		return nil, nil, err
	}
	o := &loaded
	root := &cobra.Command{
		Use: "dirwatch-cli", Short: "Watch directories and emit metadata for completed files", Version: version,
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		Example: "  dirwatch-cli\n  dirwatch-cli next --timeout 30s\n  dirwatch-cli --config ~/.config/dirwatch-cli/dirwatch.yaml",
		RunE:    func(command *cobra.Command, args []string) error { o.action = "watch"; return validate(o) },
	}
	root.CompletionOptions.DisableDefaultCmd = true
	initCommand := &cobra.Command{Use: "init", Short: "Create the default YAML configuration", Args: cobra.NoArgs,
		Run: func(command *cobra.Command, args []string) { o.action = "init" }}
	initCommand.Flags().BoolVarP(&o.initForce, "force", "f", false, "overwrite an existing configuration")
	nextCommand := &cobra.Command{Use: "next", Aliases: []string{"wait"}, Short: "Wait for ready files, claim them, then exit", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error { o.action = "next"; return validate(o) }}
	nextCommand.Flags().DurationVar(&o.lease, "lease", o.lease, "claim lease before redelivery")
	nextCommand.Flags().DurationVar(&o.waitTimeout, "timeout", 0, "maximum wait time; zero waits indefinitely")
	nextCommand.Flags().IntVar(&o.maxFiles, "max-files", o.maxFiles, "maximum number of files to claim")
	nextCommand.Flags().StringVar(&o.execCommand, "exec", "", "run a shell command with DIRWATCH_* variables and auto-confirm")
	doneCommand := eventCommand("done EVENT_ID", []string{"ack"}, "Acknowledge and archive a claimed event", "done", o)
	retryCommand := eventCommand("retry EVENT_ID", []string{"nack"}, "Release an event for immediate redelivery", "retry", o)
	statusCommand := &cobra.Command{Use: "status", Short: "Report configuration, directory access, and delivery counts", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error { o.action = "status"; return validate(o) }}
	stateCommand := &cobra.Command{Use: "state", Short: "Inspect persistent delivery state"}
	listCommand := &cobra.Command{Use: "list", Short: "List checkpoint records as JSON", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error { o.action = "state-list"; return validate(o) }}
	listCommand.Flags().StringVar(&o.stateStatus, "status", "", "filter by observing, ready, claimed, or acknowledged")
	stateCommand.AddCommand(listCommand)
	root.AddCommand(initCommand, nextCommand, doneCommand, retryCommand, statusCommand, stateCommand)
	flags := root.PersistentFlags()
	flags.StringVar(&o.configPath, "config", o.configPath, "YAML config path")
	flags.StringVar(&o.watchDir, "watch", o.watchDir, "directory to monitor")
	flags.StringVar(&o.archiveDir, "archive-dir", o.archiveDir, "archive directory")
	flags.DurationVar(&o.scanInterval, "scan-interval", o.scanInterval, "polling interval")
	flags.DurationVar(&o.inactive, "inactive", o.inactive, "required unchanged duration")
	flags.StringVar(&o.checkpoint, "checkpoint", o.checkpoint, "bbolt checkpoint path")
	flags.StringVar(&o.includeSource, "include", o.includeSource, "include regular expression")
	flags.StringVar(&o.excludeSource, "exclude", o.excludeSource, "exclude regular expression")
	root.SetArgs(args)
	root.SetOut(stderr)
	root.SetErr(stderr)
	return root, o, nil
}

func eventCommand(use string, aliases []string, short, action string, o *options) *cobra.Command {
	return &cobra.Command{Use: use, Aliases: aliases, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			o.action, o.eventID = action, args[0]
			return validate(o)
		}}
}

func validate(o *options) error {
	if o.action == "init" {
		return nil
	}
	if o.watchDir == "" || o.checkpoint == "" || o.scanInterval <= 0 || o.inactive < 0 {
		return errors.New("invalid watch, checkpoint, scan interval, or inactive setting")
	}
	if o.action == "next" && (o.lease <= 0 || o.waitTimeout < 0 || o.maxFiles <= 0) {
		return errors.New("--lease and --max-files must be positive; --timeout cannot be negative")
	}
	if o.execCommand != "" && o.maxFiles != 1 {
		return errors.New("--exec requires --max-files=1")
	}
	valid := map[string]bool{"": true, model.StatusObserving: true, model.StatusReady: true, model.StatusClaimed: true, model.StatusAcknowledged: true}
	if o.action == "state-list" && !valid[o.stateStatus] {
		return errors.New("invalid --status")
	}
	return nil
}

func compile(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile(pattern)
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root, o, err := newRoot(args, stderr)
	if err != nil {
		return err
	}
	if err := root.Execute(); err != nil {
		return err
	}
	if o.action == "" {
		return nil
	} // help or version
	if o.action == "init" {
		if err := writeInitialConfig(o.configPath, o.initForce); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "created %s\n", o.configPath)
		return err
	}
	return runAction(ctx, *o, stdout, stderr)
}

func runAction(ctx context.Context, o options, stdout, stderr io.Writer) error {
	include, err := compile(o.includeSource)
	if err != nil {
		return fmt.Errorf("invalid --include: %w", err)
	}
	exclude, err := compile(o.excludeSource)
	if err != nil {
		return fmt.Errorf("invalid --exclude: %w", err)
	}
	if o.action == "watch" || o.action == "next" {
		info, err := os.Stat(o.watchDir)
		if err != nil {
			return fmt.Errorf("watch directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("watch path is not a directory: %s", o.watchDir)
		}
	}
	o.watchDir, err = filepath.Abs(o.watchDir)
	if err != nil {
		return err
	}
	if o.archiveDir != "" {
		o.archiveDir, err = filepath.Abs(o.archiveDir)
		if err != nil {
			return err
		}
		if o.archiveDir == o.watchDir {
			return errors.New("--archive-dir must not equal --watch")
		}
	}
	store, err := checkpoint.Open(o.checkpoint)
	if err != nil {
		return fmt.Errorf("open checkpoint: %w", err)
	}
	defer store.Close()
	col := collector.New(collector.Options{WatchDir: o.watchDir, ArchiveDir: o.archiveDir, Inactive: o.inactive,
		Include: include, Exclude: exclude}, store)
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	switch o.action {
	case "done", "retry":
		op := map[string]string{"done": "ack", "retry": "nack"}[o.action]
		item, err := store.UpdateEvent(o.eventID, op, time.Now())
		if err != nil {
			return err
		}
		if o.action == "done" {
			if errs := col.Archive([]model.FileItem{item}); len(errs) > 0 {
				return errs[0]
			}
		}
		return encoder.Encode(item)
	case "state-list":
		items, err := store.List(o.stateStatus)
		if err != nil {
			return err
		}
		return encoder.Encode(items)
	case "status":
		return encoder.Encode(statusFor(o, store))
	case "next":
		return runNext(ctx, o, col, store, encoder, stderr)
	default:
		return runWatch(ctx, o, col, encoder, stderr)
	}
}

func runNext(ctx context.Context, o options, col *collector.Collector, store *checkpoint.Store, encoder *json.Encoder, stderr io.Writer) error {
	col = collector.New(collector.Options{WatchDir: o.watchDir, ArchiveDir: o.archiveDir, Inactive: o.inactive,
		Include: mustCompile(o.includeSource), Exclude: mustCompile(o.excludeSource), Lease: o.lease, MaxItems: o.maxFiles}, store)
	var timeout <-chan time.Time
	if o.waitTimeout > 0 {
		timer := time.NewTimer(o.waitTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		items, err := col.Scan()
		if err != nil {
			return err
		}
		if len(items) > 0 {
			if o.execCommand != "" {
				return runExec(ctx, o.execCommand, items[0], col, store, encoder, stderr)
			}
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

func mustCompile(pattern string) *regexp.Regexp { r, _ := compile(pattern); return r }

func runExec(ctx context.Context, script string, item model.FileItem, col *collector.Collector, store *checkpoint.Store, encoder *json.Encoder, stderr io.Writer) error {
	process := exec.CommandContext(ctx, "sh", "-c", script)
	process.Env = append(os.Environ(), "DIRWATCH_EVENT_ID="+item.EventID, "DIRWATCH_FILE_PATH="+item.FilePath, "DIRWATCH_FILE_NAME="+item.FileName)
	process.Stdout, process.Stderr = stderr, stderr
	if err := process.Run(); err != nil {
		_, _ = store.UpdateEvent(item.EventID, "nack", time.Now())
		return fmt.Errorf("exec failed; event released: %w", err)
	}
	acked, err := store.UpdateEvent(item.EventID, "ack", time.Now())
	if err != nil {
		return err
	}
	if errs := col.Archive([]model.FileItem{acked}); len(errs) > 0 {
		return errs[0]
	}
	return encoder.Encode(acked)
}

func runWatch(ctx context.Context, o options, col *collector.Collector, encoder *json.Encoder, stderr io.Writer) error {
	logger := log.New(stderr, "", log.LstdFlags)
	logger.Printf("dirwatch-cli start watch=%s scan=%s inactive=%s os=%s", o.watchDir, o.scanInterval, o.inactive, runtime.GOOS)
	for {
		items, err := col.Scan()
		if err != nil {
			logger.Printf("scan error: %v", err)
		} else {
			for _, item := range items {
				if err := encoder.Encode(item); err != nil {
					return err
				}
			}
			for _, err := range col.Archive(items) {
				logger.Printf("%v", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.scanInterval):
		}
	}
}

type statusOutput struct {
	WatchDir          string         `json:"watch_dir"`
	ArchiveDir        string         `json:"archive_dir,omitempty"`
	Checkpoint        string         `json:"checkpoint"`
	WatchAccessible   bool           `json:"watch_accessible"`
	ArchiveAccessible bool           `json:"archive_accessible"`
	States            map[string]int `json:"states"`
}

func statusFor(o options, store *checkpoint.Store) statusOutput {
	result := statusOutput{WatchDir: o.watchDir, ArchiveDir: o.archiveDir, Checkpoint: o.checkpoint,
		States: map[string]int{model.StatusObserving: 0, model.StatusReady: 0, model.StatusClaimed: 0, model.StatusAcknowledged: 0}}
	if info, err := os.Stat(o.watchDir); err == nil && info.IsDir() {
		result.WatchAccessible = true
	}
	if o.archiveDir == "" {
		result.ArchiveAccessible = true
	} else if info, err := os.Stat(o.archiveDir); err == nil && info.IsDir() {
		result.ArchiveAccessible = true
	}
	if items, err := store.List(""); err == nil {
		for _, item := range items {
			result.States[item.Status]++
		}
	}
	return result
}
