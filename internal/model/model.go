package model

import (
	"path/filepath"
	"time"
)

const (
	StatusObserving    = "observing"
	StatusReady        = "ready"
	StatusClaimed      = "claimed"
	StatusAcknowledged = "acknowledged"
	StatusDead         = "dead"
)

type FileItem struct {
	EventID    string     `json:"event_id"`
	Event      string     `json:"event"`
	Status     string     `json:"status"`
	LeaseUntil *time.Time `json:"lease_until,omitempty"`
	ReadyAt    *time.Time `json:"ready_at,omitempty"`
	Attempts   int        `json:"attempt_count"`
	LastError  string     `json:"last_error,omitempty"`
	FilePath   string     `json:"file_path"`
	FileName   string     `json:"file_name"`
	Size       int64      `json:"size"`
	MTime      time.Time  `json:"mtime"`
	Inode      uint64     `json:"inode"`
	Ext        string     `json:"ext"`
}

type CheckpointRecord struct {
	Inode         uint64    `json:"inode"`
	Size          int64     `json:"size"`
	MTime         time.Time `json:"mtime"`
	StableSince   time.Time `json:"stable_since"`
	Emitted       bool      `json:"emitted"`
	EventID       string    `json:"event_id,omitempty"`
	Status        string    `json:"status,omitempty"`
	LeaseUntil    time.Time `json:"lease_until,omitempty"`
	AckedAt       time.Time `json:"acknowledged_at,omitempty"`
	FirstSeenAt   time.Time `json:"first_seen_at,omitempty"`
	ReadyAt       time.Time `json:"ready_at,omitempty"`
	ClaimedAt     time.Time `json:"claimed_at,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
	AttemptCount  int       `json:"attempt_count,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

type StateItem struct {
	FilePath string `json:"file_path"`
	CheckpointRecord
}

func OptionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func RecordItem(path string, record CheckpointRecord) FileItem {
	return FileItem{
		EventID: record.EventID, Event: "file_ready", Status: record.Status,
		LeaseUntil: OptionalTime(record.LeaseUntil), ReadyAt: OptionalTime(record.ReadyAt),
		Attempts: record.AttemptCount, LastError: record.LastError, FilePath: path,
		FileName: filepath.Base(path), Size: record.Size, MTime: record.MTime,
		Inode: record.Inode, Ext: filepath.Ext(path),
	}
}
