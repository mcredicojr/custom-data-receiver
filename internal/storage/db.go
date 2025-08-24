package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type DB struct {
	SQL *sql.DB
}

func OpenSQLite(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	return &DB{SQL: sqlDB}, nil
}

func (db *DB) Close() error {
	if db.SQL == nil {
		return errors.New("db nil")
	}
	return db.SQL.Close()
}

func (db *DB) ApplyMigrations(dir string) error {
	if _, err := db.SQL.Exec(`CREATE TABLE IF NOT EXISTS _migrations (name TEXT PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		var exists int
		if err := db.SQL.QueryRow(`SELECT COUNT(1) FROM _migrations WHERE name = ?`, e.Name()).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		sqlBytes, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		tx, err := db.SQL.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s failed: %w", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO _migrations(name) VALUES (?)`, e.Name()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
