package native

import (
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestWorkerConfigAppliesAdoptedModel(t *testing.T) {
	base := core.Config{Model: core.ModelConfig{Model: "project-model", SmallModel: "project-small"}}
	project := core.Project{ID: "p"}
	selected, err := workerConfig(base, project, core.Session{ModelOverride: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Model.Model != "claude-opus-5-5" || selected.Model.SmallModel != "project-small" {
		t.Fatalf("selected model: %+v", selected.Model)
	}
	if base.Model.Model != "project-model" {
		t.Fatal("workerConfig mutated the shared config")
	}
}

func TestWorkerConfigProfileWinsOverAdoptedModel(t *testing.T) {
	base := core.Config{
		Model:         core.ModelConfig{Model: "project-model", SmallModel: "project-small"},
		ModelProfiles: map[string]core.ModelProfile{"careful": {Model: "careful-model"}},
	}
	project := core.Project{ID: "p"}
	selected, err := workerConfig(base, project, core.Session{Profile: "careful", ModelOverride: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Model.Model != "careful-model" {
		t.Fatalf("profile did not win: %+v", selected.Model)
	}
}

func TestWorkerConfigIgnoresInvalidAdoptedModel(t *testing.T) {
	base := core.Config{Model: core.ModelConfig{Model: "project-model"}}
	project := core.Project{ID: "p"}
	selected, err := workerConfig(base, project, core.Session{ModelOverride: "bad model"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Model.Model != "project-model" {
		t.Fatalf("invalid override was used: %+v", selected.Model)
	}
}
