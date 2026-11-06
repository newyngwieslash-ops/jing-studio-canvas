package app

import (
	"encoding/json"
	"net/http"
	"strings"

	"yingce/backend/internal/model"
	"yingce/backend/internal/production"
)

// 角色外貌重写：已建好作品圣经的作品没有“重新提取”入口，这里让模型按当前面部写法为角色
// 给出新外貌建议。建议只读，用户在弹窗里确认后由前端逐个走 UpdateDramaCast 写库。

const dramaAppearanceRewriteSource = "drama_appearance_rewrite"

type DramaAppearanceRewriteStart struct {
	TaskID string `json:"taskId"`
	// SkillName 是本次沿用的资产提取技能；为空表示用的是内置规则。
	SkillName string `json:"skillName"`
}

type DramaAppearanceProposal struct {
	CastID string `json:"castId"`
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type DramaAppearanceProposals struct {
	// Status 是 running / succeeded / failed。
	Status    string                    `json:"status"`
	Error     string                    `json:"error,omitempty"`
	Proposals []DramaAppearanceProposal `json:"proposals"`
}

func rewritableDramaCast(cast model.DramaCast) bool {
	return cast.Kind == "character" && !cast.Locked
}

// StartDramaAppearanceRewrite 为未锁定的角色创建重写任务；castID 非空时只重写这一个角色，
// 其余角色作为对照交给模型，保证新脸与他们拉开差距。
func (s *Service) StartDramaAppearanceRewrite(userID string, projectID string, castID string) (DramaAppearanceRewriteStart, error) {
	project, productionRow, err := s.dramaForUser(userID, projectID)
	if err != nil {
		return DramaAppearanceRewriteStart{}, err
	}
	casts, err := s.repo.DramaCasts(project.ID)
	if err != nil {
		return DramaAppearanceRewriteStart{}, err
	}
	castID = strings.TrimSpace(castID)
	targets, others := []production.AppearanceRewriteCharacter{}, []production.AppearanceRewriteCharacter{}
	for _, cast := range casts {
		if cast.Kind != "character" {
			continue
		}
		character := production.AppearanceRewriteCharacter{Name: cast.Name, Role: cast.Role, Description: cast.Description, Appearance: cast.Appearance}
		if rewritableDramaCast(cast) && (castID == "" || cast.ID == castID) {
			targets = append(targets, character)
		} else if castID != "" {
			others = append(others, character)
		}
	}
	if len(targets) == 0 {
		if castID != "" {
			return DramaAppearanceRewriteStart{}, BadAuthRequest("该角色不存在或已锁定形象，不能重写外貌")
		}
		return DramaAppearanceRewriteStart{}, BadAuthRequest("没有可重写外貌的角色（已锁定的角色会被跳过）")
	}
	run := &dramaRun{s: s, production: productionRow, project: project, settings: parseDramaSettings(productionRow.SettingsJSON), meta: parseDramaBibleMeta(productionRow.BibleJSON)}
	chapters, err := run.loadChapters(nil)
	if err != nil {
		return DramaAppearanceRewriteStart{}, err
	}
	skill, err := s.dramaWritingSkill(userID, run.settings.BibleSkill, "资产提取")
	if err != nil {
		return DramaAppearanceRewriteStart{}, err
	}
	prompt := production.AppearanceRewritePrompt(project.Name, run.settings.Settings, chapters, dramaBibleSampleRunes, targets, others, skill)
	input := map[string]any{
		"config":      map[string]any{"systemPrompt": production.AppearanceRewriteSystemPrompt()},
		"textOptions": map[string]any{"stream": true},
		"metadata":    map[string]any{"source": dramaAppearanceRewriteSource, "domainProjectId": project.ID, "castId": castID},
	}
	task, err := run.createTaskWith(nil, run.settings.Models.textFor(DramaStepBible), "text", "text", prompt, input)
	if err != nil {
		return DramaAppearanceRewriteStart{}, err
	}
	result := DramaAppearanceRewriteStart{TaskID: task.ID}
	if skill != nil {
		result.SkillName = skill.Name
	}
	return result, nil
}

func (s *Service) DramaAppearanceProposals(userID string, projectID string, taskID string) (DramaAppearanceProposals, error) {
	project, _, err := s.dramaForUser(userID, projectID)
	if err != nil {
		return DramaAppearanceProposals{}, err
	}
	task, err := s.Task(userID, strings.TrimSpace(taskID))
	if err != nil {
		return DramaAppearanceProposals{}, err
	}
	var input struct {
		Metadata struct {
			Source          string `json:"source"`
			DomainProjectID string `json:"domainProjectId"`
			CastID          string `json:"castId"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(task.InputJSON), &input) != nil || input.Metadata.Source != dramaAppearanceRewriteSource || input.Metadata.DomainProjectID != project.ID {
		return DramaAppearanceProposals{}, NewAppError(http.StatusNotFound, "外貌重写任务不存在")
	}
	switch task.Status {
	case model.TaskStatusSucceeded:
	case model.TaskStatusFailed, model.TaskStatusCancelled:
		return DramaAppearanceProposals{Status: "failed", Error: firstNonEmpty(task.Error, "外貌重写任务失败"), Proposals: []DramaAppearanceProposal{}}, nil
	default:
		return DramaAppearanceProposals{Status: "running", Proposals: []DramaAppearanceProposal{}}, nil
	}
	fail := func(message string) (DramaAppearanceProposals, error) {
		return DramaAppearanceProposals{Status: "failed", Error: message, Proposals: []DramaAppearanceProposal{}}, nil
	}
	text, err := dramaTaskText(task)
	if err != nil {
		return fail(err.Error())
	}
	rewrites, err := production.ParseAppearanceRewrite(text)
	if err != nil {
		return fail("外貌重写结果解析失败：" + err.Error())
	}
	casts, err := s.repo.DramaCasts(project.ID)
	if err != nil {
		return DramaAppearanceProposals{}, err
	}
	index := production.NewCastIndex()
	byID := map[string]model.DramaCast{}
	for _, cast := range casts {
		if rewritableDramaCast(cast) && (input.Metadata.CastID == "" || cast.ID == input.Metadata.CastID) {
			byID[cast.ID] = cast
			index.Add(cast.ID, append([]string{cast.Name}, decodeStringList(cast.AliasesJSON)...)...)
		}
	}
	result := DramaAppearanceProposals{Status: "succeeded", Proposals: []DramaAppearanceProposal{}}
	seen := map[string]bool{}
	for _, rewrite := range rewrites {
		id, ok := index.Lookup(rewrite.Name)
		cast := byID[id]
		after := truncateRunes(rewrite.Appearance, 1000)
		// 模型可能编造名字、重复给同一角色或输出对照角色：对不上的丢弃，没有变化的不打扰用户。
		if !ok || seen[id] || strings.TrimSpace(cast.Appearance) == after {
			continue
		}
		seen[id] = true
		result.Proposals = append(result.Proposals, DramaAppearanceProposal{CastID: id, Name: cast.Name, Before: cast.Appearance, After: after})
	}
	return result, nil
}
