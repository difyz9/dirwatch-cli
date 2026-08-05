package checkpoint

import (
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
	item, err := store.UpdateEvent("event-1", "nack", time.Now())
	if err != nil || item.Status != model.StatusReady {
		t.Fatalf("nack: %v %v", item, err)
	}
	record, _, _ = store.Get("/incoming/a.csv")
	record.Status, record.Emitted = model.StatusClaimed, true
	store.Put("/incoming/a.csv", record)
	item, err = store.UpdateEvent("event-1", "ack", time.Now())
	if err != nil || item.Status != model.StatusAcknowledged {
		t.Fatalf("ack: %v %v", item, err)
	}
}
