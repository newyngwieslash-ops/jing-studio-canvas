package production

import (
	"strings"
	"testing"
)

func TestBibleAndScriptPromptsRequireFaceSpec(t *testing.T) {
	chapters := []Chapter{{ID: "c1", Index: 1, Title: "第一章", Text: "沈彻在马厩。"}}
	for name, prompt := range map[string]string{
		"bible":   BiblePrompt("测试", Settings{}, chapters, 1000, nil),
		"script":  ScriptPrompt(ScriptContext{Title: "测试", SourceChapters: chapters}),
		"rewrite": AppearanceRewritePrompt("测试", Settings{}, chapters, 1000, []AppearanceRewriteCharacter{{Name: "沈彻", Appearance: "男，青年"}}, nil, nil),
	} {
		if !strings.Contains(prompt, "面部识别特征") || !strings.Contains(prompt, "至少 3 处明显不同") {
			t.Fatalf("%s prompt must carry the face spec rule: %s", name, prompt)
		}
	}
	if bible := BiblePrompt("测试", Settings{}, chapters, 1000, nil); !strings.Contains(bible, "不要复述视觉风格") {
		t.Fatal("visualRules must not repeat the style or prescribe one face for everyone")
	}
}

func TestCharacterTemplatesPutCharacterBeforeStyle(t *testing.T) {
	for _, scheme := range AssetSchemes() {
		name, setting, style := strings.Index(scheme.CharacterPrompt, "{{角色名称}}"), strings.Index(scheme.CharacterPrompt, "{{角色设定}}"), strings.Index(scheme.CharacterPrompt, "{{项目画风}}")
		if name < 0 || setting < name || style < setting || !strings.Contains(scheme.CharacterPrompt, "不套用画风的统一脸型") {
			t.Fatalf("%s template must lead with the character and keep faces independent of the style: %s", scheme.Mode, scheme.CharacterPrompt)
		}
	}
}

func TestParseAppearanceRewrite(t *testing.T) {
	items, err := ParseAppearanceRewrite("```json\n{\"characters\":[{\"name\":\" 沈彻 \",\"appearance\":\"男，约24岁，窄长脸\"},{\"name\":\"\",\"appearance\":\"x\"},{\"name\":\"阿绾\",\"appearance\":\"\"}]}\n```")
	if err != nil || len(items) != 1 || items[0].Name != "沈彻" {
		t.Fatalf("only complete entries survive: %+v %v", items, err)
	}
	if _, err := ParseAppearanceRewrite(`{"characters":[]}`); err == nil {
		t.Fatal("an empty result must be an error, not a silent no-op")
	}
}
