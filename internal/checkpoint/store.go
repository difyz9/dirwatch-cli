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

var ErrQueueBusy = errors.New("queue has reached max inflight")

type QueueOptions struct {
	Limit       int
	MaxInflight int
	Lease       time.Duration
	MaxAttempts int
}

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

// ClaimReady atomically requeues expired leases, enforces the global inflight
// limit, and claims ready records in deterministic FIFO order.
func (s *Store) ClaimReady(now time.Time, options QueueOptions) ([]model.FileItem, error) {
	items := make([]model.FileItem, 0)
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketName))
		type candidate struct {
			path   string
			record model.CheckpointRecord
		}
		candidates := make([]candidate, 0)
		inflight := 0
		if err := bucket.ForEach(func(key, value []byte) error {
			var record model.CheckpointRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			record.Status = effectiveStatus(record)
			changed := false
			if record.Status == model.StatusClaimed && !record.LeaseUntil.IsZero() && !now.Before(record.LeaseUntil) {
				if options.MaxAttempts > 0 && record.AttemptCount >= options.MaxAttempts {
					record.Status = model.StatusDead
					if record.LastError == "" {
						record.LastError = "claim lease expired"
					}
				} else {
					record.Status, record.Emitted = model.StatusReady, false
					record.LeaseUntil = time.Time{}
					if record.ReadyAt.IsZero() {
						record.ReadyAt = now
					}
				}
				changed = true
			}
			if record.Status == model.StatusClaimed {
				inflight++
			}
			if record.Status == model.StatusReady && (record.NextAttemptAt.IsZero() || !now.Before(record.NextAttemptAt)) {
				candidates = append(candidates, candidate{path: string(key), record: record})
			}
			if changed {
				raw, err := json.Marshal(record)
				if err != nil {
					return err
				}
				return bucket.Put(key, raw)
			}
			return nil
		}); err != nil {
			return err
		}
		if options.MaxInflight > 0 && inflight >= options.MaxInflight {
			return ErrQueueBusy
		}
		available := options.Limit
		if options.MaxInflight > 0 && available > options.MaxInflight-inflight {
			available = options.MaxInflight - inflight
		}
		sort.Slice(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.record.ReadyAt.Equal(b.record.ReadyAt) {
				return a.path < b.path
			}
			return a.record.ReadyAt.Before(b.record.ReadyAt)
		})
		for _, candidate := range candidates {
			if len(items) >= available {
				break
			}
			record := candidate.record
			record.Status, record.Emitted, record.ClaimedAt = model.StatusClaimed, true, now
			record.LeaseUntil, record.NextAttemptAt = now.Add(options.Lease), time.Time{}
			record.AttemptCount++
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(candidate.path), raw); err != nil {
				return err
			}
			items = append(items, model.RecordItem(candidate.path, record))
		}
		return nil
	})
	return items, err
}

func (s *Store) UpdateEvent(eventID, action string, now time.Time, retryDelay time.Duration, maxAttempts int, reason string) (model.FileItem, error) {
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
				record.Emitted, record.LeaseUntil, record.LastError = false, time.Time{}, reason
				if maxAttempts > 0 && record.AttemptCount >= maxAttempts {
					record.Status, record.NextAttemptAt = model.StatusDead, time.Time{}
				} else {
					record.Status, record.NextAttemptAt = model.StatusReady, now.Add(retryDelay)
					record.ReadyAt = record.NextAttemptAt
				}
			case "restore":
				if record.Status != model.StatusDead {
					return fmt.Errorf("event %s is %s, not dead", eventID, record.Status)
				}
				record.Status, record.Emitted, record.LeaseUntil = model.StatusReady, false, time.Time{}
				record.NextAttemptAt, record.LastError, record.AttemptCount = now, "", 0
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
