package checkpoint

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"dirwatch-cli/internal/model"
)

func TestAckNackLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := model.CheckpointRecord{EventID: "event-1", Status: model.StatusClaimed, Emitted: true, LeaseUntil: time.Now().Add(time.Minute)}
	if err := store.Put("/incoming/a.csv", record); err != nil {
		t.Fatal(err)
	}
	item, err := store.UpdateEvent("event-1", "nack", time.Now(), 0, 5, "test")
	if err != nil || item.Status != model.StatusReady {
		t.Fatalf("nack: %v %v", item, err)
	}
	record, _, _ = store.Get("/incoming/a.csv")
	record.Status, record.Emitted = model.StatusClaimed, true
	store.Put("/incoming/a.csv", record)
	item, err = store.UpdateEvent("event-1", "ack", time.Now(), 0, 0, "")
	if err != nil || item.Status != model.StatusAcknowledged {
		t.Fatalf("ack: %v %v", item, err)
	}
}

func TestQueueFIFOInflightRetryAndDeadLetter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	for path, readyAt := range map[string]time.Time{
		"/incoming/later.csv": now.Add(time.Second),
		"/incoming/first.csv": now,
	} {
		if err := store.Put(path, model.CheckpointRecord{EventID: filepath.Base(path), Status: model.StatusReady, ReadyAt: readyAt}); err != nil {
			t.Fatal(err)
		}
	}
	options := QueueOptions{Limit: 1, MaxInflight: 1, Lease: time.Minute, MaxAttempts: 2}
	items, err := store.ClaimReady(now.Add(2*time.Second), options)
	if err != nil || len(items) != 1 || items[0].FileName != "first.csv" {
		t.Fatalf("first claim: %#v %v", items, err)
	}
	if _, err := store.ClaimReady(now.Add(3*time.Second), options); !errors.Is(err, ErrQueueBusy) {
		t.Fatalf("second claim error=%v", err)
	}

	retried, err := store.UpdateEvent(items[0].EventID, "nack", now.Add(4*time.Second), 30*time.Second, 2, "processor failed")
	if err != nil || retried.Status != model.StatusReady || retried.LastError != "processor failed" {
		t.Fatalf("retry: %#v %v", retried, err)
	}
	next, err := store.ClaimReady(now.Add(5*time.Second), options)
	if err != nil || len(next) != 1 || next[0].FileName != "later.csv" {
		t.Fatalf("delayed FIFO: %#v %v", next, err)
	}
	if _, err := store.UpdateEvent(next[0].EventID, "ack", now.Add(6*time.Second), 0, 0, ""); err != nil {
		t.Fatal(err)
	}

	secondAttempt, err := store.ClaimReady(now.Add(35*time.Second), options)
	if err != nil || len(secondAttempt) != 1 || secondAttempt[0].Attempts != 2 {
		t.Fatalf("second attempt: %#v %v", secondAttempt, err)
	}
	dead, err := store.UpdateEvent(secondAttempt[0].EventID, "nack", now.Add(36*time.Second), 0, 2, "failed again")
	if err != nil || dead.Status != model.StatusDead {
		t.Fatalf("dead: %#v %v", dead, err)
	}
	restored, err := store.UpdateEvent(dead.EventID, "restore", now.Add(37*time.Second), 0, 0, "")
	if err != nil || restored.Status != model.StatusReady || restored.Attempts != 0 {
		t.Fatalf("restore: %#v %v", restored, err)
	}
}
