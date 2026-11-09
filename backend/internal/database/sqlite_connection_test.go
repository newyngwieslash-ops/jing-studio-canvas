package database

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
)

func TestOpenSQLiteAppliesPragmasAndBusyTimeout(t *testing.T) {
	db, err := Open(Config{Driver: "sqlite", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := ConfigurePool(db); err != nil {
		t.Fatalf("configure pool: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn1, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("conn1: %v", err)
	}
	defer conn1.Close()
	conn2, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("conn2: %v", err)
	}
	defer conn2.Close()

	for idx, conn := range []*struct {
		name  string
		query func(string, ...any) error
	}{
		{
			name: "conn1",
			query: func(q string, dest ...any) error {
				return conn1.QueryRowContext(ctx, q).Scan(dest...)
			},
		},
		{
			name: "conn2",
			query: func(q string, dest ...any) error {
				return conn2.QueryRowContext(ctx, q).Scan(dest...)
			},
		},
	} {
		var busyTimeout int
		if err := conn.query("PRAGMA busy_timeout;", &busyTimeout); err != nil {
			t.Fatalf("%s query busy_timeout: %v", conn.name, err)
		}
		if busyTimeout != 5000 {
			t.Fatalf("%s busy_timeout = %d, want 5000 (idx=%d)", conn.name, busyTimeout, idx)
		}

		var journalMode string
		if err := conn.query("PRAGMA journal_mode;", &journalMode); err != nil {
			t.Fatalf("%s query journal_mode: %v", conn.name, err)
		}
		if strings.ToLower(journalMode) != "wal" {
			t.Fatalf("%s journal_mode = %q, want wal", conn.name, journalMode)
		}

		var foreignKeys int
		if err := conn.query("PRAGMA foreign_keys;", &foreignKeys); err != nil {
			t.Fatalf("%s query foreign_keys: %v", conn.name, err)
		}
		if foreignKeys != 1 {
			t.Fatalf("%s foreign_keys = %d, want 1", conn.name, foreignKeys)
		}

		var synchronous int
		if err := conn.query("PRAGMA synchronous;", &synchronous); err != nil {
			t.Fatalf("%s query synchronous: %v", conn.name, err)
		}
		// NORMAL in SQLite is 1
		if synchronous != 1 {
			t.Fatalf("%s synchronous = %d, want 1 (NORMAL)", conn.name, synchronous)
		}
	}

	if err := db.Exec("CREATE TABLE concurrent_probe (id INTEGER PRIMARY KEY, counter INTEGER NOT NULL)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec("INSERT INTO concurrent_probe (id, counter) VALUES (1, 0)").Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}

	const workers = 8
	const iterations = 15
	errCh := make(chan error, workers*iterations)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if txErr := db.Transaction(func(tx *gorm.DB) error {
					return tx.Exec("UPDATE concurrent_probe SET counter = counter + 1 WHERE id = 1").Error
				}); txErr != nil {
					errCh <- txErr
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for txErr := range errCh {
		t.Fatalf("concurrent transaction failed: %v", txErr)
	}

	var total int
	if err := db.Raw("SELECT counter FROM concurrent_probe WHERE id = 1").Scan(&total).Error; err != nil {
		t.Fatalf("read total: %v", err)
	}
	if total != workers*iterations {
		t.Fatalf("counter = %d, want %d", total, workers*iterations)
	}
}
