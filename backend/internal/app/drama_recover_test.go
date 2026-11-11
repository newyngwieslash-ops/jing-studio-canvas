package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yingce/backend/internal/model"
	"yingce/backend/internal/production"
)

// 参考图“已生成但保存失败”时：收件箱标记可恢复，恢复沿用原任务重新保存，成功后成为当前参考图且不重复记账。
func TestRecoverDramaCastImageReusesSavedTask(t *testing.T) {
	svc, fake := newDramaTestService(t)
	svc.dataDir = t.TempDir()
	// 预置设定密钥：首次生成密钥要同步目录，Windows 上对临时目录 fsync 会被拒绝。
	if err := os.WriteFile(filepath.Join(svc.dataDir, ".settings-key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := DramaSettings{ReviewMode: DramaReviewAuto, MediaMode: DramaMediaManual, Concurrency: 16, Models: fakeDramaModels, EpisodeMode: DramaEpisodeModeChapter, ChaptersPerEpisode: 1}
	settings.EpisodeSeconds = 60
	settings.Storyboard.SegmentMode, settings.Storyboard.SegmentSeconds = production.SegmentFixed, 5
	summary, err := svc.CreateDrama("usr-drama", CreateDramaRequest{Name: "恢复", Start: true, Settings: settings, Chapters: []DramaChapterInput{{Title: "第一章", Text: "萧炎……"}}})
	if err != nil {
		t.Fatal(err)
	}
	project := summary.Project.ID
	runDramaTicks(t, svc, project, 20)
	var hero model.DramaCast
	casts, _ := svc.repo.DramaCasts(project)
	for _, cast := range casts {
		if cast.Kind == "character" && cast.ParentID == "" {
			hero = cast
			break
		}
	}
	if _, err := svc.RegenerateDramaCast("usr-drama", project, hero.ID, false); err != nil {
		t.Fatal(err)
	}
	runDramaTicks(t, svc, project, 4)
	job, err := svc.repo.LatestDramaCastJob(project, hero.ID)
	if err != nil || job.Status != model.DramaJobSucceeded || job.TaskID == "" {
		t.Fatalf("cast job not finished: %+v %v", job, err)
	}
	task, err := svc.repo.Task(job.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	resourceID := dramaTaskResource(task)
	// 模拟“作品已生成但登记失败”：任务失败并留有恢复信息，作业与设定标记失败。
	checkpoint := `{"mode":"image","items":[{"reference":{"url":"https://example.test/result"},"resourceId":"` + resourceID + `"}],"startedAt":"2026-01-01T00:00:00Z"}`
	if err := fake.db.Model(&model.Task{}).Where("id = ?", task.ID).Updates(map[string]any{"status": model.TaskStatusFailed, "media_recovery_json": checkpoint, "media_stage": "register", "error": "作品已生成，但保存未完成：登记作品失败"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fake.db.Model(&model.DramaJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": model.DramaJobFailed, "error": "作品已生成，但保存未完成：登记作品失败"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.repo.UpdateDramaCastFields(hero.ID, map[string]any{"image_status": model.DramaMediaFailed, "image_resource_id": ""}); err != nil {
		t.Fatal(err)
	}
	attemptsBefore := job.AttemptsJSON

	view, err := svc.DramaSummary("usr-drama", project)
	if err != nil {
		t.Fatal(err)
	}
	recoverable := false
	for _, item := range view.Inbox {
		if item.CastID == hero.ID && item.Kind == "cast_failed" {
			recoverable = item.Recoverable
		}
	}
	if !recoverable {
		t.Fatalf("saved-but-unregistered cast must be marked recoverable: %+v", view.Inbox)
	}
	if _, err := svc.RecoverDramaCastImage("someone", project, hero.ID); err == nil {
		t.Fatal("recover must check ownership")
	}
	result, err := svc.RecoverDramaCastImage("usr-drama", project, hero.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result["taskId"] != task.ID {
		t.Fatalf("recovery must reuse the original task: %+v", result)
	}
	requeued, _ := svc.repo.Task(task.ID)
	if requeued.Status != model.TaskStatusQueued {
		t.Fatalf("task should be requeued for saving, got %s", requeued.Status)
	}
	imagesBefore := fake.byKind["image"]
	// 模拟保存成功。
	if err := fake.db.Model(&model.Task{}).Where("id = ?", task.ID).Updates(map[string]any{"status": model.TaskStatusSucceeded, "result_json": task.ResultJSON}).Error; err != nil {
		t.Fatal(err)
	}
	runDramaTicks(t, svc, project, 3)
	if fake.byKind["image"] != imagesBefore {
		t.Fatal("recovery must not generate again")
	}
	updated, _ := svc.repo.DramaCast(project, hero.ID)
	if updated.ImageStatus != model.DramaMediaReady || updated.ImageResourceID != resourceID {
		t.Fatalf("recovered image must become current: %+v", updated)
	}
	after, _ := svc.repo.LatestDramaCastJob(project, hero.ID)
	if after.ID != job.ID || after.Status != model.DramaJobSucceeded || after.AttemptsJSON != attemptsBefore {
		t.Fatalf("job must finish without a second billing attempt: %+v", after)
	}
	if _, err := svc.RecoverDramaCastImage("usr-drama", project, hero.ID); err == nil {
		t.Fatal("ready cast has nothing to recover")
	}
}

// 保存失败原因写日志前去掉数据目录、URL 查询串与凭据。
func TestMediaFailureDetailRedactsPathsAndSecrets(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{dataDir: dir}
	cause := errors.New("open " + filepath.Join(dir, "media-staging", "result-1") + ": database is locked; https://oss.example.test/a.png?token=abc Authorization: Bearer secret-k")
	detail := svc.mediaFailureDetail(cause)
	if strings.Contains(detail, dir) || strings.Contains(detail, "token=abc") || strings.Contains(detail, "secret-k") {
		t.Fatalf("detail leaks sensitive text: %s", detail)
	}
	if !strings.Contains(detail, "[数据目录]") || !strings.Contains(detail, "database is locked") {
		t.Fatalf("detail lost the useful cause: %s", detail)
	}
}
