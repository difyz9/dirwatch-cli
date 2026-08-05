package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"dirwatch-cli/internal/model"
	bolt "go.etcd.io/bbolt"
)

const bucketName = "spool_state" // Kept for compatibility with existing databases.

type Store struct{ db *bolt.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Get(path string) (model.CheckpointRecord, bool, error) {
	var record model.CheckpointRecord
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketName)).Get([]byte(path))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &record)
	})
	return record, found, err
}

func (s *Store) Put(path string, record model.CheckpointRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketName)).Put([]byte(path), raw)
	})
}

func (s *Store) Delete(path string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketName)).Delete([]byte(path))
	})
}

func (s *Store) List(status string) ([]model.StateItem, error) {
	items := make([]model.StateItem, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketName)).ForEach(func(key, value []byte) error {
			var record model.CheckpointRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			record.Status = effectiveStatus(record)
			if status == "" || status == record.Status {
				items = append(items, model.StateItem{FilePath: string(key), CheckpointRecord: record})
			}
			return nil
		})
	})
	sort.Slice(items, func(i, j int) bool { return items[i].FilePath < items[j].FilePath })
	return items, err
}

func effectiveStatus(record model.CheckpointRecord) string {
	if record.Status != "" {
		return record.Status
	}
	if record.Emitted {
		return model.StatusAcknowledged
	}
	return model.StatusObserving
}

func (s *Store) UpdateEvent(eventID, action string, now time.Time) (model.FileItem, error) {
	var item model.FileItem
	found := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketName))
		cursor := bucket.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var record model.CheckpointRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.EventID != eventID {
				continue
			}
			found = true
			switch action {
			case "ack":
				if record.Status != model.StatusClaimed && record.Status != model.StatusAcknowledged {
					return fmt.Errorf("event %s is %s, not claimed", eventID, record.Status)
				}
				record.Status, record.AckedAt, record.LeaseUntil = model.StatusAcknowledged, now, time.Time{}
			case "nack":
				if record.Status != model.StatusClaimed {
					return fmt.Errorf("event %s is %s, not claimed", eventID, record.Status)
				}
				record.Emitted, record.Status, record.LeaseUntil = false, model.StatusReady, time.Time{}
			default:
				return fmt.Errorf("unknown event action %q", action)
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			item = model.RecordItem(string(key), record)
			return bucket.Put(key, raw)
		}
		return nil
	})
	if err != nil {
		return item, err
	}
	if !found {
		return item, fmt.Errorf("event not found: %s", eventID)
	}
	return item, nil
}

func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
