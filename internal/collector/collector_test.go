package collector

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/difyz9/dmon-cli/internal/checkpoint"
)

func TestStableLeaseAndRedelivery(t *testing.T) {
	watch := t.TempDir()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	col := New(Options{WatchDir: watch, Inactive: time.Second, Include: regexp.MustCompile(`\.csv$`), Lease: 5 * time.Second}, store)
	col.SetClock(func() time.Time { return now })
	os.WriteFile(filepath.Join(watch, "a.csv"), []byte("data"), 0o644)
	if items, _ := col.Scan(); len(items) != 0 {
		t.Fatalf("first scan: %v", items)
	}
	now = now.Add(time.Second)
	first, err := col.Scan()
	if err != nil || len(first) != 1 || first[0].EventID == "" {
		t.Fatalf("claim: %v %v", first, err)
	}
	now = now.Add(5 * time.Second)
	second, err := col.Scan()
	if err != nil || len(second) != 1 || second[0].EventID != first[0].EventID {
		t.Fatalf("redelivery: %v %v", second, err)
	}
}

func TestRecursiveArchive(t *testing.T) {
	watch := t.TempDir()
	archive := filepath.Join(watch, "archive")
	store, _ := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	defer store.Close()
	now := time.Now()
	col := New(Options{WatchDir: watch, ArchiveDir: archive, Include: regexp.MustCompile(`\.mp4$`)}, store)
	col.SetClock(func() time.Time { return now })
	nested := filepath.Join(watch, "camera")
	os.MkdirAll(nested, 0o755)
	os.WriteFile(filepath.Join(nested, "a.mp4"), []byte("video"), 0o644)
	col.Scan()
	now = now.Add(time.Second)
	items, _ := col.Scan()
	if len(items) != 1 {
		t.Fatalf("items: %v", items)
	}
	if errs := col.Archive(items); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, err := os.Stat(filepath.Join(archive, "camera", "a.mp4")); err != nil {
		t.Fatal(err)
	}
	if items, err := col.Scan(); err != nil || len(items) != 0 {
		t.Fatalf("archive rescanned: %v %v", items, err)
	}
}
