package app

import (
	"encoding/json"
	"strings"
	"testing"

	"yingce/backend/internal/model"
)

func TestDramaAppearanceRewriteProposals(t *testing.T) {
	svc, fake := newDramaTestService(t)
	projectID := createTestDrama(t, svc, DramaReviewAuto).Project.ID
	runDramaTicks(t, svc, projectID, 6)
	if err := fake.db.Model(&model.DramaCast{}).Where("project_id = ? AND name = ?", projectID, "纳兰嫣然").Update("locked", true).Error; err != nil {
		t.Fatal(err)
	}
	var prompt string
	svc.dramaTaskHook = func(userID string, req CreateTaskRequest, timeline *renderProject) (*model.Task, error) {
		task, err := fake.submit(userID, req, timeline)
		if err != nil || req.Type != "canvas_text" || !strings.Contains(req.Prompt, "【角色列表】") {
			return task, err
		}
		prompt = req.Prompt
		input, _ := json.Marshal(req.Input)
		// 别名命中萧炎；锁定角色、编造的名字、没有变化的结果都不应出现在建议里。
		text := `{"characters":[{"name":"炎儿","appearance":"男，约16岁，窄长脸尖下颌，剑眉，单眼皮细长眼，高窄鼻，薄唇，左眉尾有浅疤"},` +
			`{"name":"纳兰嫣然","appearance":"女，约17岁，圆脸"},{"name":"不存在的人","appearance":"男，约30岁"}]}`
		result, _ := json.Marshal(map[string]any{"mode": "text", "text": text})
		return task, fake.db.Model(task).Updates(map[string]any{"input_json": string(input), "result_json": string(result)}).Error
	}

	started, err := svc.StartDramaAppearanceRewrite("usr-drama", projectID, "")
	if err != nil {
		t.Fatal(err)
	}
	// 原文抽样里会出现锁定角色的名字，只检查角色列表段。
	list := strings.SplitN(prompt, "【小说原文抽样】", 2)[0]
	if !strings.Contains(list, "萧炎") || strings.Contains(list, "纳兰嫣然") {
		t.Fatalf("locked characters must not be rewritten: %s", list)
	}
	got, err := svc.DramaAppearanceProposals("usr-drama", projectID, started.TaskID)
	if err != nil || got.Status != "succeeded" || len(got.Proposals) != 1 {
		t.Fatalf("exactly one proposal expected: %+v %v", got, err)
	}
	if proposal := got.Proposals[0]; proposal.Name != "萧炎" || !strings.Contains(proposal.After, "左眉尾有浅疤") || proposal.Before == "" || proposal.CastID == "" {
		t.Fatalf("proposal must map back to the cast: %+v", proposal)
	}
	if _, err := svc.DramaAppearanceProposals("someone", projectID, started.TaskID); err == nil {
		t.Fatal("another user must not read the proposals")
	}

	// 单个角色：只重写药老，萧炎作为对照写进提示词；模型顺带返回的萧炎会被丢弃。
	var yaoID string
	if err := fake.db.Model(&model.DramaCast{}).Where("project_id = ? AND name = ?", projectID, "药老").Pluck("id", &yaoID).Error; err != nil || yaoID == "" {
		t.Fatalf("seed cast 药老 missing: %v", err)
	}
	single, err := svc.StartDramaAppearanceRewrite("usr-drama", projectID, yaoID)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(prompt, "【其他角色】", 2)
	if len(parts) != 2 || !strings.Contains(parts[0], "药老") || strings.Contains(parts[0], "萧炎") || !strings.Contains(strings.SplitN(parts[1], "【小说原文抽样】", 2)[0], "萧炎") {
		t.Fatalf("single rewrite must target one cast and list the rest as contrast: %s", prompt)
	}
	got, err = svc.DramaAppearanceProposals("usr-drama", projectID, single.TaskID)
	if err != nil || got.Status != "succeeded" || len(got.Proposals) != 0 {
		t.Fatalf("contrast characters must never become proposals: %+v %v", got, err)
	}
	if _, err := svc.StartDramaAppearanceRewrite("usr-drama", projectID, "missing"); err == nil {
		t.Fatal("unknown cast must be rejected")
	}
}
