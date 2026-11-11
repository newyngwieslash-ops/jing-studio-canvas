package repository

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
)

func TestClaimNextTaskUsesActiveClaimPartialIndex(t *testing.T) {
	db, err := database.Open(database.Config{
		Driver: "sqlite",
		DSN:    "file:repo-claim-active-partial-index?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MigrateSchema(db); err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-10 * time.Minute)
	expiredLease := base.Add(-time.Minute)
	activeLease := time.Now().Add(10 * time.Minute)
	futurePoll := time.Now().Add(10 * time.Minute)

	historical := make([]model.Task, 0, 300)
	for i := range 300 {
		status := model.TaskStatusSucceeded
		switch i % 3 {
		case 1:
			status = model.TaskStatusFailed
		case 2:
			status = model.TaskStatusCancelled
		}
		historical = append(historical, model.Task{
			ID:        fmt.Sprintf("term-%d", i),
			UserID:    "u1",
			Status:    status,
			CreatedAt: base.Add(-time.Duration(400-i) * time.Second),
		})
	}
	if err := db.CreateInBatches(&historical, 150).Error; err != nil {
		t.Fatal(err)
	}

	fixtures := []model.Task{
		{ID: "running-busy", UserID: "u1", Status: model.TaskStatusRunning, LeaseOwner: "other", LeaseExpiresAt: &activeLease, CreatedAt: base.Add(4 * time.Second)},
		{ID: "queued-deferred", UserID: "u1", Status: model.TaskStatusQueued, NextPollAt: &futurePoll, CreatedAt: base.Add(5 * time.Second)},
		{ID: "queued-oldest", UserID: "u1", Status: model.TaskStatusQueued, CreatedAt: base.Add(6 * time.Second)},
		{ID: "running-expired", UserID: "u1", Status: model.TaskStatusRunning, LeaseOwner: "dead-worker", LeaseExpiresAt: &expiredLease, CreatedAt: base.Add(7 * time.Second)},
	}
	for i := range 100 {
		if i%2 == 0 {
			fixtures = append(fixtures, model.Task{
				ID:             fmt.Sprintf("running-inflight-%d", i),
				UserID:         "u1",
				Status:         model.TaskStatusRunning,
				LeaseOwner:     "worker-live",
				LeaseExpiresAt: &activeLease,
				CreatedAt:      base.Add(time.Duration(10+i) * time.Second),
			})
		} else {
			fixtures = append(fixtures, model.Task{
				ID:         fmt.Sprintf("queued-future-poll-%d", i),
				UserID:     "u1",
				Status:     model.TaskStatusQueued,
				NextPollAt: &futurePoll,
				CreatedAt:  base.Add(time.Duration(10+i) * time.Second),
			})
		}
	}
	if err := db.CreateInBatches(&fixtures, 150).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ANALYZE").Error; err != nil {
		t.Fatal(err)
	}

	var capturedSQL string
	var capturedVars []any
	const hookName = "test:capture_claim_next_task_select"
	if err := db.Callback().Query().After("gorm:query").Register(hookName, func(tx *gorm.DB) {
		sqlStr := tx.Statement.SQL.String()
		if capturedSQL == "" && strings.Contains(sqlStr, "ORDER BY created_at asc") {
			capturedSQL = sqlStr
			capturedVars = append([]any(nil), tx.Statement.Vars...)
		}
	}); err != nil {
		t.Fatal(err)
	}

	repo := New(db)
	first, err := repo.ClaimNextTask("worker-a", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first ClaimNextTask = %v, err = %v", first, err)
	}
	if first.ID != "queued-oldest" {
		t.Fatalf("first claimed task ID = %q, want %q", first.ID, "queued-oldest")
	}
	if capturedSQL == "" {
		t.Fatal("did not capture ClaimNextTask SELECT query")
	}

	type queryPlanRow struct {
		ID      int
		Parent  int
		NotUsed int
		Detail  string
	}
	var plan []queryPlanRow
	if err := db.Raw("EXPLAIN QUERY PLAN "+capturedSQL, capturedVars...).Scan(&plan).Error; err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN failed for %q: %v", capturedSQL, err)
	}
	usedPartialIndex := false
	var details []string
	for _, row := range plan {
		details = append(details, row.Detail)
		if strings.Contains(row.Detail, "idx_tasks_active_claim") {
			usedPartialIndex = true
		}
		if strings.Contains(row.Detail, "USE TEMP B-TREE FOR ORDER BY") {
			t.Fatalf("ClaimNextTask SELECT %q used temp B-tree sort; plan = %v", capturedSQL, details)
		}
	}
	if !usedPartialIndex {
		t.Fatalf("ClaimNextTask SELECT %q did not use idx_tasks_active_claim; plan = %v", capturedSQL, details)
	}

	second, err := repo.ClaimNextTask("worker-b", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second ClaimNextTask = %v, err = %v", second, err)
	}
	if second.ID != "running-expired" {
		t.Fatalf("second claimed task ID = %q, want %q", second.ID, "running-expired")
	}

	third, err := repo.ClaimNextTask("worker-c", time.Minute)
	if err != nil || third != nil {
		t.Fatalf("third ClaimNextTask = %v, err = %v, want nil, nil", third, err)
	}
}
