package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestYAMLConfigAndCLIOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := []byte("watch: /from/yaml\narchive_dir: /archive\nscan_interval: 7s\ninactive: 9s\ncheckpoint: /tmp/yaml.db\ninclude: '\\.png$'\nexclude: ''\n")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags([]string{"--config", configPath, "--inactive", "1s", "--watch", "/from/cli"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if o.watchDir != "/from/cli" || o.archiveDir != "/archive" || o.scanInterval != 7*time.Second || o.inactive != time.Second || o.includeSource != `\.png$` || o.excludeSource != "" {
		t.Fatalf("options = %+v", o)
	}
}

func TestExplicitMissingConfigFails(t *testing.T) {
	_, err := parseFlags([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml")}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected missing explicit config to fail")
	}
}

func newTestScanner(t *testing.T, dir string, inactive time.Duration, now *time.Time) *scanner {
	t.Helper()
	db, err := openCheckpoint(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &scanner{
		dir: dir, inactive: inactive, include: regexp.MustCompile(`\.csv$|\.jpg$|\.mp4$`),
		exclude: regexp.MustCompile(`\.tmp$|\.part$`), db: db, now: func() time.Time { return *now },
	}
}

func TestScanStableAndNoDuplicate(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	s := newTestScanner(t, dir, 3*time.Second, &now)
	path := filepath.Join(dir, "a.csv")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.scan(); err != nil || len(got) != 0 {
		t.Fatalf("first scan = %v, %v", got, err)
	}
	now = now.Add(2 * time.Second)
	if got, _ := s.scan(); len(got) != 0 {
		t.Fatalf("emitted too early: %v", got)
	}
	now = now.Add(time.Second)
	got, err := s.scan()
	if err != nil || len(got) != 1 || got[0].FileName != "a.csv" || got[0].Size != 3 {
		t.Fatalf("stable scan = %v, %v", got, err)
	}
	now = now.Add(10 * time.Second)
	if got, _ := s.scan(); len(got) != 0 {
		t.Fatalf("duplicate output: %v", got)
	}
}

func TestChangedFileRestartsTimer(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s := newTestScanner(t, dir, 3*time.Second, &now)
	path := filepath.Join(dir, "movie.mp4")
	os.WriteFile(path, []byte("a"), 0o644)
	s.scan()
	now = now.Add(2 * time.Second)
	os.WriteFile(path, []byte("longer"), 0o644)
	if got, _ := s.scan(); len(got) != 0 {
		t.Fatal("changed file was emitted")
	}
	now = now.Add(3 * time.Second)
	got, err := s.scan()
	if err != nil || len(got) != 1 || got[0].Size != 6 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFiltersAndCleanupRemoved(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s := newTestScanner(t, dir, 0, &now)
	os.WriteFile(filepath.Join(dir, "keep.jpg"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "skip.jpg.part"), []byte("x"), 0o644)
	s.scan()
	now = now.Add(time.Second)
	got, err := s.scan()
	if err != nil || len(got) != 1 || got[0].FileName != "keep.jpg" {
		t.Fatalf("got %v, %v", got, err)
	}
	if err := os.Remove(filepath.Join(dir, "keep.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.scan(); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.View(func(tx *bolt.Tx) error {
		count = tx.Bucket([]byte(stateBucket)).Stats().KeyN
		return nil
	})
	if count != 0 {
		t.Fatalf("checkpoint count = %d", count)
	}
}

func TestRecursiveScan(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s := newTestScanner(t, dir, 0, &now)
	nested := filepath.Join(dir, "tenant", "day")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "new.csv"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.scan()
	now = now.Add(time.Second)
	got, err := s.scan()
	if err != nil || len(got) != 1 || got[0].FilePath != filepath.Join(nested, "new.csv") {
		t.Fatalf("recursive scan = %v, %v", got, err)
	}
}

func TestArchivePreservesRelativeDirectory(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive")
	now := time.Now()
	s := newTestScanner(t, dir, 0, &now)
	s.archive = archive
	sourceDir := filepath.Join(dir, "camera-1")
	os.MkdirAll(sourceDir, 0o755)
	source := filepath.Join(sourceDir, "clip.mp4")
	os.WriteFile(source, []byte("video"), 0o644)
	s.scan()
	now = now.Add(time.Second)
	items, err := s.scan()
	if err != nil || len(items) != 1 {
		t.Fatalf("scan = %v, %v", items, err)
	}
	if errs := s.archiveItems(items); len(errs) != 0 {
		t.Fatalf("archive errors: %v", errs)
	}
	destination := filepath.Join(archive, "camera-1", "clip.mp4")
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("archived file: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	// The archive lives below watch but must never be emitted again.
	now = now.Add(time.Second)
	if got, err := s.scan(); err != nil || len(got) != 0 {
		t.Fatalf("archive was rescanned: %v, %v", got, err)
	}
}
