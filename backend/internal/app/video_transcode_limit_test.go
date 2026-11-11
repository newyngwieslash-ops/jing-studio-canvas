package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

const (
	fakeFfmpegModeEnv      = "CANVAS_TEST_FAKE_FFMPEG_MODE"
	fakeFfmpegRecordOutEnv = "CANVAS_TEST_FAKE_FFMPEG_RECORD_OUT"
	fakeFfmpegReadyFileEnv = "CANVAS_TEST_FAKE_FFMPEG_READY_FILE"
)

func init() {
	mode := os.Getenv(fakeFfmpegModeEnv)
	if mode == "" {
		return
	}
	outPath := os.Args[len(os.Args)-1]
	if recordPath := os.Getenv(fakeFfmpegRecordOutEnv); recordPath != "" {
		_ = os.WriteFile(recordPath, []byte(outPath), 0o600)
	}
	switch mode {
	case "success":
		if _, err := os.Stat(outPath); err == nil {
			_, _ = os.Stderr.WriteString("stale output file was not cleaned before ffmpeg started")
			os.Exit(2)
		}
		if err := os.WriteFile(outPath, []byte("published-h264-mp4"), 0o644); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		os.Exit(0)
	case "fail":
		_ = os.WriteFile(outPath, []byte("corrupt-partial-avc1"), 0o644)
		_, _ = os.Stderr.WriteString("simulated libx264 failure")
		os.Exit(1)
	case "hang":
		_ = os.WriteFile(outPath, []byte("in-progress-partial-avc1"), 0o644)
		if readyFile := os.Getenv(fakeFfmpegReadyFileEnv); readyFile != "" {
			_ = os.WriteFile(readyFile, []byte("ready"), 0o600)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func newTranscodeLimitTestService(t *testing.T, concurrency int) (*Service, *gorm.DB) {
	t.Helper()
	db, err := database.Open(database.Config{
		Driver: "sqlite",
		DSN:    "file:" + newID() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.Resource{}, &model.SystemSetting{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	var sem chan struct{}
	if concurrency > 0 {
		sem = make(chan struct{}, concurrency)
	}
	svc := &Service{
		repo:         repository.New(db),
		dataDir:      t.TempDir(),
		transcodeSem: sem,
	}
	if err := os.WriteFile(filepath.Join(svc.dataDir, ".settings-key"), make([]byte, 32), 0o600); err != nil {
		t.Fatalf("seed settings key: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, db
}

func TestParseTranscodeConcurrency(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 1},
		{"   ", 1},
		{"invalid", 1},
		{"0", 1},
		{"-2", 1},
		{"1", 1},
		{"2", 2},
		{" 3 ", 3},
		{"4", 4},
		{"5", 4},
		{"10", 4},
	}
	for _, tc := range cases {
		if got := parseTranscodeConcurrency(tc.raw); got != tc.want {
			t.Errorf("parseTranscodeConcurrency(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestAcquireTranscodeSlotBoundsConcurrencyAndHonorsCancel(t *testing.T) {
	// Zero-value Service literal compatibility when transcodeSem is nil.
	zeroSvc := &Service{}
	zeroRelease, zeroOK := zeroSvc.acquireTranscodeSlot(context.Background())
	if !zeroOK || zeroRelease == nil {
		t.Fatalf("zero-value Service acquireTranscodeSlot: ok=%v releaseNil=%v, want true/false", zeroOK, zeroRelease == nil)
	}
	zeroRelease()
	zeroRelease()

	for _, limit := range []int{1, 2} {
		svc := &Service{transcodeSem: make(chan struct{}, limit)}
		const totalWorkers = 6
		var active atomic.Int32
		var maxActive atomic.Int32
		var completed atomic.Int32
		firstWave := make(chan struct{}, limit)
		releaseFirstWave := make(chan struct{})

		var wg sync.WaitGroup
		for i := 0; i < totalWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				release, ok := svc.acquireTranscodeSlot(context.Background())
				if !ok {
					t.Errorf("limit=%d expected acquire ok", limit)
					return
				}
				cur := active.Add(1)
				for {
					prev := maxActive.Load()
					if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
						break
					}
				}
				select {
				case firstWave <- struct{}{}:
					<-releaseFirstWave
				default:
					time.Sleep(15 * time.Millisecond)
				}
				active.Add(-1)
				completed.Add(1)
				// Idempotent release: calling twice must not free another worker's slot.
				release()
				release()
			}()
		}

		for i := 0; i < limit; i++ {
			select {
			case <-firstWave:
			case <-time.After(2 * time.Second):
				t.Fatalf("limit=%d timed out waiting for %d initial workers", limit, limit)
			}
		}
		// Give queued goroutines a chance to contend on the full semaphore.
		time.Sleep(25 * time.Millisecond)
		if got := int(active.Load()); got != limit {
			t.Fatalf("limit=%d active while saturated = %d, want %d", limit, got, limit)
		}
		close(releaseFirstWave)
		wg.Wait()

		if got := int(completed.Load()); got != totalWorkers {
			t.Fatalf("limit=%d completed = %d, want %d", limit, got, totalWorkers)
		}
		if got := int(maxActive.Load()); got != limit {
			t.Fatalf("limit=%d maxActive = %d, want %d", limit, got, limit)
		}
	}

	// Cancellation unblocks waiting caller immediately and preserves held slot.
	svc := &Service{transcodeSem: make(chan struct{}, 1)}
	holdRelease, ok := svc.acquireTranscodeSlot(context.Background())
	if !ok {
		t.Fatal("failed to acquire initial slot")
	}
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitDone := make(chan bool, 1)
	go func() {
		_, acquired := svc.acquireTranscodeSlot(waitCtx)
		waitDone <- acquired
	}()
	time.Sleep(20 * time.Millisecond)
	waitCancel()

	select {
	case acquired := <-waitDone:
		if acquired {
			t.Fatal("canceled waiter unexpectedly acquired slot")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not unblock promptly")
	}

	holdRelease()
	nextRelease, ok := svc.acquireTranscodeSlot(context.Background())
	if !ok {
		t.Fatal("expected slot to be reusable after holder release")
	}
	nextRelease()
}

func TestRunPlaybackTranscodeShutdownRequeuesProcessingResource(t *testing.T) {
	svc, db := newTranscodeLimitTestService(t, 1)

	seedClaimed := func(id string) string {
		t.Helper()
		src := filepath.Join(svc.dataDir, "resources", "clips", id+".mp4")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := model.Resource{
			ID:             id,
			UserID:         "user-1",
			Kind:           "video",
			Status:         model.ResourceStatusReady,
			Provider:       "local",
			ObjectKey:      "clips/" + id + ".mp4",
			PlaybackStatus: model.PlaybackStatusProcessing,
		}
		if err := db.Create(&res).Error; err != nil {
			t.Fatal(err)
		}
		return src
	}

	srcInflight := seedClaimed("res-inflight")
	srcWaiting := seedClaimed("res-waiting")

	inflightEntered := make(chan struct{})
	svc.transcodeRunner = func(ctx context.Context, src string, dst string) error {
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte("partial-mp4"), 0o644); err != nil {
			return err
		}
		close(inflightEntered)
		<-ctx.Done()
		return ctx.Err()
	}

	go svc.runPlaybackTranscode("user-1", "res-inflight", srcInflight)
	select {
	case <-inflightEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight transcode to start")
	}

	go svc.runPlaybackTranscode("user-1", "res-waiting", srcWaiting)
	// Allow res-waiting to enter runPlaybackTranscode and block on transcodeSem.
	time.Sleep(30 * time.Millisecond)

	// Close must cancel transcodeContext and join all transcode goroutines before returning.
	if err := svc.Close(); err != nil {
		t.Fatalf("svc.Close() error = %v", err)
	}

	for _, id := range []string{"res-inflight", "res-waiting"} {
		var got model.Resource
		if err := db.First(&got, "id = ?", id).Error; err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		if got.PlaybackStatus != "" {
			t.Fatalf("%s PlaybackStatus = %q, want empty string for clean re-claim", id, got.PlaybackStatus)
		}
		if got.PlaybackError != "" {
			t.Fatalf("%s PlaybackError = %q, want empty", id, got.PlaybackError)
		}
	}

	partialDst := filepath.Join(svc.dataDir, playbackDirName, "res-inflight.mp4")
	if _, err := os.Stat(partialDst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial playback file %s still exists (stat err=%v), want removed", partialDst, err)
	}
}

func TestRunPlaybackTranscodeStopWorkerAndNewServiceEnv(t *testing.T) {
	t.Setenv(transcodeConcurrencyEnv, "3")
	db, err := database.Open(database.Config{
		Driver: "sqlite",
		DSN:    "file:" + newID() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.Resource{}, &model.SystemSetting{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	svc := New(repository.New(db), t.TempDir())
	t.Cleanup(func() { _ = svc.Close() })
	if got := cap(svc.transcodeSem); got != 3 {
		t.Fatalf("cap(svc.transcodeSem) = %d, want 3", got)
	}

	// Build minimal hvc1 mp4 so maybeStartPlaybackTranscode claims and spawns runPlaybackTranscode.
	stsd := stsdBoxWithFourcc("hvc1")
	moov := make([]byte, 8+len(stsd))
	moov[3] = byte(len(moov))
	copy(moov[4:8], "moov")
	copy(moov[8:], stsd)
	ftyp := []byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0}
	srcRel := "clips/stopworker-h265.mp4"
	srcPath := filepath.Join(svc.dataDir, "resources", filepath.FromSlash(srcRel))
	if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, append(ftyp, moov...), 0o644); err != nil {
		t.Fatal(err)
	}
	res := model.Resource{
		ID:        "stopworker-h265",
		UserID:    "user-1",
		Kind:      "video",
		Status:    model.ResourceStatusReady,
		Provider:  "local",
		ObjectKey: srcRel,
	}
	if err := db.Create(&res).Error; err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	svc.transcodeRunner = func(ctx context.Context, src string, dst string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}

	svc.maybeStartPlaybackTranscode(&res)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for maybeStartPlaybackTranscode runner")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := svc.StopWorker(stopCtx); err != nil {
		t.Fatalf("StopWorker error = %v", err)
	}

	var got model.Resource
	if err := db.First(&got, "id = ?", "stopworker-h265").Error; err != nil {
		t.Fatal(err)
	}
	if got.PlaybackStatus != "" {
		t.Fatalf("PlaybackStatus after StopWorker = %q, want empty", got.PlaybackStatus)
	}
}

func TestValidateSaaSStorageReadiness(t *testing.T) {
	if err := (*Service)(nil).ValidateSaaSStorageReadiness(false); err != nil {
		t.Fatalf("nil service with requireObjectStorage=false returned error: %v", err)
	}
	if err := (&Service{}).ValidateSaaSStorageReadiness(true); err == nil {
		t.Fatal("expected error when repo is nil and requireObjectStorage=true")
	}

	svc, _ := newTranscodeLimitTestService(t, 1)

	if err := svc.ValidateSaaSStorageReadiness(false); err != nil {
		t.Fatalf("requireObjectStorage=false without OSS returned error: %v", err)
	}
	if err := svc.ValidateSaaSStorageReadiness(true); err == nil || !strings.Contains(err.Error(), "CANVAS_REQUIRE_OBJECT_STORAGE") {
		t.Fatalf("requireObjectStorage=true without OSS = %v, want CANVAS_REQUIRE_OBJECT_STORAGE error", err)
	}

	saveSystemOSS := func(value ossSettingValue) {
		t.Helper()
		encrypted, err := svc.encryptOSSSettingSecrets(value)
		if err != nil {
			t.Fatalf("encryptOSSSettingSecrets: %v", err)
		}
		raw, err := json.Marshal(encrypted)
		if err != nil {
			t.Fatalf("marshal oss setting: %v", err)
		}
		if err := svc.repo.SaveSystemSetting(&model.SystemSetting{
			Key:       ossSettingKey,
			ValueJSON: string(raw),
		}); err != nil {
			t.Fatalf("SaveSystemSetting: %v", err)
		}
	}

	saveSystemOSS(ossSettingValue{
		Enabled:         false,
		Provider:        "aliyun",
		Region:          "cn-hangzhou",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "canvas-prod",
		AccessKeyID:     "ak-disabled",
		AccessKeySecret: "sk-disabled",
	})
	if err := svc.ValidateSaaSStorageReadiness(true); err == nil {
		t.Fatal("expected error when system OSS is disabled")
	}

	saveSystemOSS(ossSettingValue{
		Enabled:         true,
		Provider:        "aliyun",
		Region:          "cn-hangzhou",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "canvas-prod",
		AccessKeyID:     "ak-live",
		AccessKeySecret: "sk-live",
	})
	if err := svc.ValidateSaaSStorageReadiness(true); err != nil {
		t.Fatalf("requireObjectStorage=true with valid aliyun OSS returned error: %v", err)
	}

	// Private-proxy object storage (no CDN, HTTP internal origin) warns but still passes readiness.
	saveSystemOSS(ossSettingValue{
		Enabled:         true,
		Provider:        "s3",
		Region:          "us-east-1",
		Endpoint:        "http://minio.internal:9000",
		Bucket:          "canvas-private",
		AccessKeyID:     "ak-minio",
		AccessKeySecret: "sk-minio",
	})
	if err := svc.ValidateSaaSStorageReadiness(true); err != nil {
		t.Fatalf("requireObjectStorage=true with private S3 origin returned error: %v", err)
	}
}

func TestRunH264TranscodeAtomicPublishAndCleanup(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(renderFfmpegEnv, exe)

	workDir := t.TempDir()
	src := filepath.Join(workDir, "input.mp4")
	if err := os.WriteFile(src, []byte("raw-hevc"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Non-zero exit writes only to dst+".tmp.mp4", cleans it up, and never leaves dst on disk.
	failDst := filepath.Join(workDir, "playback", "res-fail.mp4")
	failRecord := filepath.Join(workDir, "record-fail.txt")
	t.Setenv(fakeFfmpegModeEnv, "fail")
	t.Setenv(fakeFfmpegRecordOutEnv, failRecord)
	t.Setenv(fakeFfmpegReadyFileEnv, "")
	err = runH264Transcode(context.Background(), src, failDst)
	if err == nil || !strings.Contains(err.Error(), "simulated libx264 failure") {
		t.Fatalf("runH264Transcode(fail) error = %v, want simulated libx264 failure", err)
	}
	gotArg, readErr := os.ReadFile(failRecord)
	if readErr != nil {
		t.Fatalf("read recorded output arg: %v", readErr)
	}
	if string(gotArg) != failDst+".tmp.mp4" {
		t.Fatalf("ffmpeg output arg = %q, want %q", string(gotArg), failDst+".tmp.mp4")
	}
	if _, statErr := os.Stat(failDst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final dst %s exists after failed transcode (stat err=%v)", failDst, statErr)
	}
	if _, statErr := os.Stat(failDst + ".tmp.mp4"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temp dst %s still exists after failed transcode (stat err=%v)", failDst+".tmp.mp4", statErr)
	}

	// 2. Context cancellation mid-run kills ffmpeg, removes .tmp.mp4, and leaves dst absent.
	cancelDst := filepath.Join(workDir, "playback", "res-cancel.mp4")
	readyFile := filepath.Join(workDir, "hang-ready.txt")
	t.Setenv(fakeFfmpegModeEnv, "hang")
	t.Setenv(fakeFfmpegRecordOutEnv, "")
	t.Setenv(fakeFfmpegReadyFileEnv, readyFile)
	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- runH264Transcode(ctx, src, cancelDst)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, statErr := os.Stat(readyFile); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for fake ffmpeg hang process to start")
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	select {
	case cancelErr := <-runErrCh:
		if cancelErr == nil {
			t.Fatal("expected error from canceled runH264Transcode")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled runH264Transcode did not exit promptly")
	}
	if _, statErr := os.Stat(cancelDst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final dst %s exists after canceled transcode (stat err=%v)", cancelDst, statErr)
	}
	if _, statErr := os.Stat(cancelDst + ".tmp.mp4"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temp dst %s still exists after canceled transcode (stat err=%v)", cancelDst+".tmp.mp4", statErr)
	}

	// 3. Success cleans up pre-existing stale .tmp.mp4 upfront and atomically renames to dst.
	okDst := filepath.Join(workDir, "playback", "res-ok.mp4")
	if err := os.MkdirAll(filepath.Dir(okDst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(okDst+".tmp.mp4", []byte("stale-orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	okRecord := filepath.Join(workDir, "record-ok.txt")
	t.Setenv(fakeFfmpegModeEnv, "success")
	t.Setenv(fakeFfmpegRecordOutEnv, okRecord)
	t.Setenv(fakeFfmpegReadyFileEnv, "")
	if err := runH264Transcode(context.Background(), src, okDst); err != nil {
		t.Fatalf("runH264Transcode(success) error = %v", err)
	}
	gotOkArg, readErr := os.ReadFile(okRecord)
	if readErr != nil {
		t.Fatalf("read recorded ok arg: %v", readErr)
	}
	if string(gotOkArg) != okDst+".tmp.mp4" {
		t.Fatalf("ffmpeg output arg = %q, want %q", string(gotOkArg), okDst+".tmp.mp4")
	}
	if _, statErr := os.Stat(okDst + ".tmp.mp4"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temp dst %s still exists after successful rename (stat err=%v)", okDst+".tmp.mp4", statErr)
	}
	published, err := os.ReadFile(okDst)
	if err != nil {
		t.Fatalf("read published dst: %v", err)
	}
	if string(published) != "published-h264-mp4" {
		t.Fatalf("published dst content = %q, want %q", string(published), "published-h264-mp4")
	}
}
