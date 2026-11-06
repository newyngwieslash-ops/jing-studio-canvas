package skills

import (
	"strings"
	"testing"
)

func TestBuiltinFaceDifferentiationSkillLoads(t *testing.T) {
	packages, err := loadBuiltinSkillPackagesFromFS(builtinSkillFiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range packages {
		if item.skill.ID != "yingce-drama-face-differentiation" {
			continue
		}
		body, ok := item.archive.Files["SKILL.md"]
		if item.skill.Tag != "drama" || !ok || !strings.Contains(string(body), "差异矩阵") {
			t.Fatalf("face differentiation skill is incomplete: tag=%s files=%d", item.skill.Tag, len(item.archive.Files))
		}
		return
	}
	t.Fatal("builtin skill yingce-drama-face-differentiation was not loaded")
}
