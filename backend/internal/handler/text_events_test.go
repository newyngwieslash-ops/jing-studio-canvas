package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yingce/backend/internal/auth"
	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
	"yingce/backend/internal/service"

	"github.com/gin-gonic/gin"
)

func TestTaskTextEventCursorPrefersQueryAndSupportsLastEventID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest("GET", "/api/tasks/task-1/text-events?after=7", nil)
	request.Header.Set("Last-Event-ID", "3")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request
	after, err := taskTextEventCursor(context)
	if err != nil || after != 7 {
		t.Fatalf("cursor = %d, err = %v", after, err)
	}

	request = httptest.NewRequest("GET", "/api/tasks/task-1/text-events", nil)
	request.Header.Set("Last-Event-ID", "3")
	context, _ = gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request
	after, err = taskTextEventCursor(context)
	if err != nil || after != 3 {
		t.Fatalf("Last-Event-ID cursor = %d, err = %v", after, err)
	}
}

func TestTaskTextEventCursorRejectsInvalidValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, query := range []string{"after=abc", "after=-1"} {
		t.Run(query, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/api/tasks/task-1/text-events?"+query, nil)
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = request
			if after, err := taskTextEventCursor(context); err == nil || after != 0 {
				t.Fatalf("cursor = %d, err = %v; want a rejected cursor", after, err)
			}
		})
	}
}

func TestStreamTaskTextEventsCachedReplayReachesTerminalWithoutDuplicateDelta(t *testing.T) {
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
	if err := db.AutoMigrate(&model.Task{}, &model.TaskTextDelta{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Task{ID: "task", UserID: "user", Status: model.TaskStatusRunning}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.TaskTextDelta{ID: "delta", UserID: "user", TaskID: "task", Sequence: 8, Content: "text", ExpiresAt: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	svc := service.New(repository.New(db), t.TempDir())
	t.Cleanup(func() { _ = svc.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	initial, err := svc.CachedTaskTextReplay(ctx, "user", "task", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Task{}).Where("id = ?", "task").Updates(map[string]any{"status": model.TaskStatusSucceeded, "progress": 100}).Error; err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest("GET", "/api/tasks/task/text-events", nil).WithContext(ctx)
	streamTaskTextEvents(c, svc, "user", "task", 7, initial, nil)
	body := response.Body.String()
	if strings.Count(body, "id: 8\nevent: delta") != 1 || !strings.Contains(body, "event: terminal") || !strings.Contains(body, `"progress":100`) {
		t.Fatalf("unexpected cached SSE stream: %q", body)
	}
}

func TestStreamTaskTextEventsWritesSequenceAndTerminalEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest("GET", "/api/tasks/task-1/text-events", nil)
	replay := &service.TextReplayResult{
		Deltas:    []model.TaskTextDelta{{Sequence: 8, Content: "增量"}},
		Complete:  true,
		FinalText: "增量",
		Status:    model.TaskStatusSucceeded,
		Stage:     "已完成",
		Progress:  100,
	}
	streamTaskTextEvents(context, nil, "user-1", "task-1", 7, replay, nil)
	if response.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if !strings.Contains(body, "event: progress") || !strings.Contains(body, `"progress":100`) || !strings.Contains(body, "id: 8\nevent: delta") || !strings.Contains(body, "event: terminal") {
		t.Fatalf("unexpected SSE body: %q", body)
	}
}

func TestTaskTextEventsWakeDeliversDeltaImmediately(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	gin.SetMode(gin.TestMode)
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
	for _, row := range []any{
		&model.User{ID: "sse-user", Username: "sse-user", Role: model.UserRoleUser, Status: model.UserStatusActive},
		&model.AuthSession{ID: "sse-session", UserID: "sse-user", TokenHash: auth.HashToken("sse-token"), ExpiresAt: time.Now().Add(time.Hour)},
		&model.Task{ID: "wake-task-1", UserID: "sse-user", Type: "canvas_text", Status: model.TaskStatusTextReplay, Stage: "生成中"},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := service.New(repository.New(db), t.TempDir())
	t.Cleanup(func() { _ = svc.Close() })
	router := gin.New()
	RegisterCanvasAPI(router.Group("/api"), svc)
	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tasks/wake-task-1/text-events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "sse-session.sse-token"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	readUntilSSELine(t, reader, "event: progress")
	readUntilSSELine(t, reader, "data: ")

	// Immediately append delta while textReplayReadCache still holds the 750ms cached empty slice!
	deltaStart := time.Now()
	if _, err := svc.AppendTaskTextDelta("sse-user", "wake-task-1", "唤醒增量"); err != nil {
		t.Fatalf("AppendTaskTextDelta: %v", err)
	}
	readUntilSSELine(t, reader, "event: delta")
	dataLine := readUntilSSELine(t, reader, "data: ")
	if elapsed := time.Since(deltaStart); elapsed >= 200*time.Millisecond {
		t.Fatalf("task text SSE delta latency = %s, want < 200ms (must wake immediately and bypass stale cache)", elapsed)
	}
	if !strings.Contains(dataLine, "唤醒增量") {
		t.Fatalf("unexpected delta payload: %q", dataLine)
	}

	termStart := time.Now()
	if _, err := svc.CompleteTextReplayTask("sse-user", "wake-task-1", "唤醒增量"); err != nil {
		t.Fatalf("CompleteTextReplayTask: %v", err)
	}
	readUntilSSELine(t, reader, "event: terminal")
	if elapsed := time.Since(termStart); elapsed >= 200*time.Millisecond {
		t.Fatalf("task text SSE terminal latency = %s, want < 200ms", elapsed)
	}
}

func TestTaskTextEventsInitialReadBypassesStaleIncompleteCache(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	gin.SetMode(gin.TestMode)
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
	for _, row := range []any{
		&model.User{ID: "sse-tab-user", Username: "sse-tab-user", Role: model.UserRoleUser, Status: model.UserStatusActive},
		&model.AuthSession{ID: "sse-tab-session", UserID: "sse-tab-user", TokenHash: auth.HashToken("sse-tab-token"), ExpiresAt: time.Now().Add(time.Hour)},
		&model.Task{ID: "stale-cache-task", UserID: "sse-tab-user", Type: "canvas_text", Status: model.TaskStatusTextReplay, Stage: "生成中"},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := service.New(repository.New(db), t.TempDir())
	t.Cleanup(func() { _ = svc.Close() })
	router := gin.New()
	RegisterCanvasAPI(router.Group("/api"), svc)
	srv := httptest.NewServer(router)
	defer srv.Close()

	// Tab 1 warms key ("sse-tab-user", "stale-cache-task", 0) while Complete == false.
	warmed, err := svc.CachedTaskTextReplay(context.Background(), "sse-tab-user", "stale-cache-task", 0)
	if err != nil || warmed.Complete {
		t.Fatalf("warmed = %+v, err = %v; want Complete=false", warmed, err)
	}

	// Task finishes BEFORE Tab 2 subscribes (so Notify already fired before Tab 2 calls SubscribeWake).
	if _, err := svc.CompleteTextReplayTask("sse-tab-user", "stale-cache-task", "已完成正文"); err != nil {
		t.Fatalf("CompleteTextReplayTask: %v", err)
	}

	// Tab 2 connects within the 750ms cache TTL with after=0; it must NOT stall on the stale cached entry.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tasks/stale-cache-task/text-events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "sse-tab-session.sse-tab-token"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	readUntilSSELine(t, reader, "event: terminal")
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("new tab terminal SSE latency = %s, want < 200ms (initial snapshot must read fresh DB state after SubscribeWake)", elapsed)
	}
}

func TestAgentRunEventsWakeDeliversEventImmediately(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	gin.SetMode(gin.TestMode)

	releaseAgent := make(chan struct{})
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		select {
		case <-releaseAgent:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(releaseAgent)
		agentServer.Close()
	})
	t.Setenv("YINGCE_AGENT_URL", agentServer.URL)
	t.Setenv("YINGCE_AGENT_TOKEN", strings.Repeat("k", 32))

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

	const userID = "agent-sse-user"
	capCfgBytes, err := json.Marshal(service.DefaultModelCapabilityConfigForModel(string(model.ChannelInterfaceChatCompletion), "text-test"))
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range []any{
		&model.User{ID: userID, Username: userID, Role: model.UserRoleUser, Status: model.UserStatusActive},
		&model.AuthSession{ID: "agent-sse-session", UserID: userID, TokenHash: auth.HashToken("agent-sse-token"), ExpiresAt: time.Now().Add(time.Hour)},
		&model.ModelChannel{ID: "channel", Scope: model.ChannelScopeSystem, Enabled: true, Name: "受控测试"},
		&model.ChannelModel{ID: "cm", ChannelID: "channel", ModelKey: "text-test", Capability: "text", Protocol: model.ChannelInterfaceChatCompletion, CapabilityConfigJSON: string(capCfgBytes), BillingMode: "fixed_request", UnitPriceMicrocredits: 100, PriceConfigured: true, Enabled: true},
		&model.ChannelModelPriceTier{ID: "tier", ChannelModelID: "cm", SelectorKey: "{}", SelectorJSON: "{}", BillingMode: "fixed_request", UnitPriceMicrocredits: 100, PriceConfigured: true, Enabled: true},
		&model.CreditAccount{UserID: userID, AvailableMicrocredits: 10000},
		&model.CanvasProject{ID: "agent-canvas", UserID: userID, PayloadJSON: `{"nodes":[]}`},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := service.New(repository.New(db), t.TempDir())
	t.Cleanup(func() { _ = svc.Close() })
	previous := runtimeService
	ConfigureRuntime(svc)
	t.Cleanup(func() { ConfigureRuntime(previous) })

	runReq := service.CloudAgentRequest{
		CanvasID:        "agent-canvas",
		Prompt:          "测试流式唤醒",
		Model:           "text-test",
		ChannelID:       "channel",
		ChannelModelKey: "text-test",
		PermissionMode:  "read_only",
		ContextScope:    []string{"canvas"},
		IdempotencyKey:  "agent-sse-key-1",
	}
	runReq.Budget.MaxCredits = 1
	createdRun, err := svc.CreateCloudAgentRun(userID, runReq, "")
	if err != nil {
		t.Fatalf("CreateCloudAgentRun: %v", err)
	}
	runID := createdRun.ID

	router := gin.New()
	RegisterAgentRoutes(router.Group("/api"), svc)
	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/agent/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "agent-sse-session.agent-sse-token"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	readUntilSSELine(t, reader, "event: run_snapshot")
	readUntilSSELine(t, reader, "data: ")

	wakeStart := time.Now()
	if _, err := svc.InterjectCloudAgent(userID, runID, "msg-wake-1", "立即送达插话"); err != nil {
		t.Fatalf("InterjectCloudAgent: %v", err)
	}
	readUntilSSELine(t, reader, "event: agent_event")
	dataLine := readUntilSSELine(t, reader, "data: ")
	if elapsed := time.Since(wakeStart); elapsed >= 200*time.Millisecond {
		t.Fatalf("agent SSE event latency = %s, want < 200ms (must wake immediately instead of waiting for 5s poll)", elapsed)
	}
	if !strings.Contains(dataLine, "立即送达插话") {
		t.Fatalf("unexpected agent_event payload: %q", dataLine)
	}
}

func TestTaskTextRetryWithBillingEmitsWake(t *testing.T) {
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

	task := model.Task{
		ID:     "retry-wake-task",
		UserID: "retry-user",
		Type:   "canvas_text",
		Status: model.TaskStatusFailed,
		Stage:  "生成失败",
		Error:  "upstream timeout",
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}

	notified := make(chan string, 2)
	repo := repository.New(db).WithWakeNotifier(func(topic string) {
		notified <- topic
	})
	retried, err := repo.RetryTaskWithBilling("retry-user", &task, nil, 10)
	if err != nil {
		t.Fatalf("RetryTaskWithBilling: %v", err)
	}
	if retried.Status != model.TaskStatusQueued {
		t.Fatalf("retried.Status = %q, want %q", retried.Status, model.TaskStatusQueued)
	}
	select {
	case topic := <-notified:
		if topic != "task-text:retry-wake-task" {
			t.Fatalf("wake topic = %q, want %q", topic, "task-text:retry-wake-task")
		}
	default:
		t.Fatal("expected RetryTaskWithBilling to emit wake notification for task-text:retry-wake-task")
	}
}

func readUntilSSELine(t *testing.T, reader *bufio.Reader, prefix string) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, prefix) {
				ch <- result{line: trimmed}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("reading SSE prefix %q failed: %v", prefix, res.err)
		}
		return res.line
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for SSE prefix %q", prefix)
		return ""
	}
}
