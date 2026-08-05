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
)

type FileItem struct {
	EventID    string     `json:"event_id"`
	Event      string     `json:"event"`
	Status     string     `json:"status"`
	LeaseUntil *time.Time `json:"lease_until,omitempty"`
	FilePath   string     `json:"file_path"`
	FileName   string     `json:"file_name"`
	Size       int64      `json:"size"`
	MTime      time.Time  `json:"mtime"`
	Inode      uint64     `json:"inode"`
	Ext        string     `json:"ext"`
}

type CheckpointRecord struct {
	Inode       uint64    `json:"inode"`
	Size        int64     `json:"size"`
	MTime       time.Time `json:"mtime"`
	StableSince time.Time `json:"stable_since"`
	Emitted     bool      `json:"emitted"`
	EventID     string    `json:"event_id,omitempty"`
	Status      string    `json:"status,omitempty"`
	LeaseUntil  time.Time `json:"lease_until,omitempty"`
	AckedAt     time.Time `json:"acknowledged_at,omitempty"`
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
		LeaseUntil: OptionalTime(record.LeaseUntil), FilePath: path,
		FileName: filepath.Base(path), Size: record.Size, MTime: record.MTime,
		Inode: record.Inode, Ext: filepath.Ext(path),
	}
}
