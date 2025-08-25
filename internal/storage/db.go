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

// OpenSQLite opens (or creates) a SQLite database file with sane defaults
// for concurrency in Go. It enables WAL, foreign keys, and sets a busy_timeout
// so concurrent inserts won't fail with SQLITE_BUSY. It also serializes access
// through a single pooled connection, which avoids writer thrash.
func OpenSQLite(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	// DSN notes:
	//   - journal_mode(WAL): better read/write concurrency
	//   - foreign_keys(ON): enforce foreign key constraints
	//   - busy_timeout(5000): wait up to 5s when the database is locked
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
		path,
	)

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Serialize access. SQLite allows only one writer; multiple pooled conns can
	// increase contention and SQLITE_BUSY errors under load.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// Belt-and-suspenders: reaffirm PRAGMAs even if DSN changes later.
	if _, err := sqlDB.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	// Reasonable durability with good throughput under WAL.
	_, _ = sqlDB.Exec(`PRAGMA synchronous = NORMAL;`)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
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
	if _, err := db.SQL.Exec(`
		CREATE TABLE IF NOT EXISTS _migrations (
			name TEXT PRIMARY KEY,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
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
		if err := db.SQL.QueryRow(
			`SELECT COUNT(1) FROM _migrations WHERE name = ?`, e.Name(),
		).Scan(&exists); err != nil {
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
