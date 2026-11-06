package production

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 角色外貌重写：作品圣经建好后，用当前的面部写法重新生成角色 appearance，
// 供已有作品在不重做整本圣经的前提下消除“同一张脸”。

// AppearanceRewriteCharacter 是交给模型的现有角色设定。
type AppearanceRewriteCharacter struct {
	Name        string
	Role        string
	Description string
	Appearance  string
}

// AppearanceRewrite 是模型对单个角色给出的新外貌。
type AppearanceRewrite struct {
	Name       string `json:"name"`
	Appearance string `json:"appearance"`
}

func AppearanceRewriteSystemPrompt() string {
	return "你是资深短剧美术指导，负责让每个角色拥有可被图片模型区分的脸。" + jsonOnly
}

// AppearanceRewritePrompt 与建圣经时共用原文抽样，保证原文写明的面部特征不丢。
// others 是不需要重写、但新外貌必须与之拉开差距的角色（单个角色重写时为其余角色）。
func AppearanceRewritePrompt(title string, settings Settings, chapters []Chapter, maxChars int, characters []AppearanceRewriteCharacter, others []AppearanceRewriteCharacter, skill *WritingSkill) string {
	settings = settings.Normalized()
	var b strings.Builder
	fmt.Fprintf(&b, "作品：%s\n视觉风格（仅供判断时代与造型，不要复述）：%s\n\n", title, firstNonEmpty(settings.StylePrompt, "写实电影感"))
	b.WriteString(structuredSkillBlock("重写角色外貌", skill))
	b.WriteString("请为下面【角色列表】里的每个角色重写 appearance，输出：\n")
	b.WriteString(`{"characters":[{"name":"与角色列表完全一致的名字","appearance":"新的外貌描述"}]}` + "\n")
	b.WriteString("规则：单人角色的 appearance 以性别和具体年龄（如“男，约24岁”）开头，然后按面部写法描述。" + FaceSpecRule + "\n")
	b.WriteString("原有外貌中的服装、配饰、体型可沿用；不要改变人物身份、关系和时代背景。\n\n")
	writeAppearanceCharacters(&b, "【角色列表】", characters)
	if len(others) > 0 {
		writeAppearanceCharacters(&b, "\n【其他角色】（不要输出，新外貌必须与他们明显不同）", others)
	}
	b.WriteString("\n【小说原文抽样】\n")
	b.WriteString(SampleNovel(chapters, maxChars))
	return b.String()
}

func writeAppearanceCharacters(b *strings.Builder, title string, characters []AppearanceRewriteCharacter) {
	b.WriteString(title + "\n")
	for _, character := range characters {
		fmt.Fprintf(b, "- %s｜定位：%s｜身份性格：%s｜现有外貌：%s\n", character.Name, firstNonEmpty(character.Role, "未标注"), firstNonEmpty(truncateRunes(character.Description, 200), "未标注"), truncateRunes(character.Appearance, 300))
	}
}

// ParseAppearanceRewrite 丢弃没有名字或外貌为空的条目。
func ParseAppearanceRewrite(raw string) ([]AppearanceRewrite, error) {
	text, err := ExtractJSONObject(raw)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Characters []AppearanceRewrite `json:"characters"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, err
	}
	result := make([]AppearanceRewrite, 0, len(payload.Characters))
	for _, item := range payload.Characters {
		item.Name, item.Appearance = strings.TrimSpace(item.Name), strings.TrimSpace(item.Appearance)
		if item.Name != "" && item.Appearance != "" {
			result = append(result, item)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("模型没有返回可用的角色外貌")
	}
	return result, nil
}
