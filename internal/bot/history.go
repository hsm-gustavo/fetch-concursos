package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func openHistory(path string) (*sql.DB, error) {
	if path == "" {
		path = "data/history.sqlite"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create history directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			metadata_json TEXT NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS events_item_time ON events(item_id, occurred_at)",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize history: %w", err)
		}
	}
	return db, nil
}

func (s *Service) recordEvent(ctx context.Context, itemID, eventType string, metadata map[string]string) {
	if s.history == nil {
		return
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		s.logger.Error("failed to encode history event", "error", err)
		return
	}
	_, err = s.history.ExecContext(ctx, "INSERT INTO events(item_id, event_type, occurred_at, metadata_json) VALUES (?, ?, ?, ?)", itemID, eventType, time.Now().UTC().Format(time.RFC3339Nano), string(data))
	if err != nil {
		s.logger.Error("failed to record history event", "type", eventType, "error", err)
	}
}
