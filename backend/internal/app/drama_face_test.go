package app

import (
	"strings"
	"testing"

	"yingce/backend/internal/model"
	"yingce/backend/internal/production"
)

// 角色本体指定人脸参考后：解锁并以人脸参考做图生图，提示词仍带外貌设定；造型不能单独指定人脸。
func TestDramaCastFaceReference(t *testing.T) {
	svc, fake := newDramaTestService(t)
	settings := DramaSettings{ReviewMode: DramaReviewAuto, MediaMode: DramaMediaManual, Concurrency: 16, Models: fakeDramaModels, EpisodeMode: DramaEpisodeModeChapter, ChaptersPerEpisode: 1}
	settings.EpisodeSeconds = 60
	settings.Storyboard.SegmentMode, settings.Storyboard.SegmentSeconds = production.SegmentFixed, 5
	summary, err := svc.CreateDrama("usr-drama", CreateDramaRequest{Name: "人脸", Start: true, Settings: settings, Chapters: []DramaChapterInput{{Title: "第一章", Text: "萧炎……"}}})
	if err != nil {
		t.Fatal(err)
	}
	project := summary.Project.ID
	runDramaTicks(t, svc, project, 20)
	casts, err := svc.repo.DramaCasts(project)
	if err != nil {
		t.Fatal(err)
	}
	var hero model.DramaCast
	for _, cast := range casts {
		if cast.Kind == "character" && cast.ParentID == "" {
			hero = cast
			break
		}
	}
	if hero.ID == "" {
		t.Fatalf("no base character: %+v", casts)
	}
	if err := svc.repo.UpdateDramaCastFields(hero.ID, map[string]any{"locked": true}); err != nil {
		t.Fatal(err)
	}
	face := fake.resource("usr-drama", "image", "image/png")
	if _, err := svc.SetDramaCastFace("usr-drama", project, hero.ID, SetDramaCastFaceRequest{ResourceID: fake.resource("someone", "image", "image/png")}); err == nil {
		t.Fatal("face image must belong to the user")
	}
	look, err := svc.AddDramaCastLook("usr-drama", project, hero.ID, DramaCastLookRequest{LookName: "内门弟子", Appearance: "青色劲装"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDramaCastFace("usr-drama", project, look["look"].(model.DramaCast).ID, SetDramaCastFaceRequest{ResourceID: face}); err == nil {
		t.Fatal("looks cannot have their own face reference")
	}
	if _, err := svc.SetDramaCastFace("usr-drama", project, hero.ID, SetDramaCastFaceRequest{ResourceID: face}); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.repo.DramaCast(project, hero.ID)
	if err != nil || updated.FaceResourceID != face || updated.Locked || updated.ImageStatus != model.DramaMediaQueued {
		t.Fatalf("face must be stored, unlock and requeue: %+v %v", updated, err)
	}
	before := len(fake.requests)
	runDramaTicks(t, svc, project, 4)
	var found *CreateTaskRequest
	for i := before; i < len(fake.requests); i++ {
		req := fake.requests[i]
		refs, _ := req.Input["referenceImages"].([]any)
		if req.Type == "canvas_image" && len(refs) == 1 && refs[0].(map[string]any)["storageKey"] == "resource:"+face {
			found = &fake.requests[i]
			break
		}
	}
	if found == nil {
		t.Fatal("base character image must reference the face image")
	}
	if found.Operation != "image_to_image" || !strings.Contains(found.Prompt, "人脸参考") || !strings.Contains(found.Prompt, firstNonEmpty(hero.Appearance, hero.Description, hero.Role)) {
		t.Fatalf("face request must be image_to_image and keep the appearance: %s %q", found.Operation, found.Prompt)
	}

	if _, err := svc.SetDramaCastFace("usr-drama", project, hero.ID, SetDramaCastFaceRequest{}); err != nil {
		t.Fatal(err)
	}
	if cleared, _ := svc.repo.DramaCast(project, hero.ID); cleared.FaceResourceID != "" {
		t.Fatal("empty resource must clear the face reference")
	}
}
