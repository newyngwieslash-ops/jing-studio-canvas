package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

func TestStructLiteralServiceEnsureWakeHubDoesNotMutateRepoAndEmitsFallbackWakes(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	db, err := database.Open(database.Config{Driver: "sqlite", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigurePool(db); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.MigrateSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Task{
		ID:     "literal-task",
		UserID: "literal-user",
		Type:   "canvas_text",
		Status: model.TaskStatusTextReplay,
		Stage:  "生成中",
	}).Error; err != nil {
		t.Fatal(err)
	}

	origRepo := repository.New(db)
	svc := &Service{repo: origRepo}
	t.Cleanup(func() { _ = svc.Close() })

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, unsub := svc.SubscribeWake("task-text:literal-task")
			unsub()
		}()
		go func() {
			defer wg.Done()
			if _, err := svc.CachedTaskTextReplay(context.Background(), "literal-user", "literal-task", 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if svc.repo != origRepo || svc.repo.HasWakeNotifier() {
		t.Fatalf("ensureWakeHub mutated struct-literal Service.repo (got %p hasWake=%v, want %p hasWake=false)", svc.repo, svc.repo.HasWakeNotifier(), origRepo)
	}

	wakeCh, unsub := svc.SubscribeWake("task-text:literal-task")
	defer unsub()

	if _, err := svc.AppendTaskTextDelta("literal-user", "literal-task", "片段一"); err != nil {
		t.Fatalf("AppendTaskTextDelta: %v", err)
	}
	select {
	case <-wakeCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for notifyWakeFallback on AppendTaskTextDelta")
	}

	if _, err := svc.CompleteTextReplayTask("literal-user", "literal-task", "片段一完整"); err != nil {
		t.Fatalf("CompleteTextReplayTask: %v", err)
	}
	select {
	case <-wakeCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for notifyWakeFallback on CompleteTextReplayTask")
	}

	constructed := New(repository.New(db), t.TempDir())
	t.Cleanup(func() { _ = constructed.Close() })
	if !constructed.repo.HasWakeNotifier() {
		t.Fatal("expected New() to wire repo.WithWakeNotifier before publishing Service")
	}
}
