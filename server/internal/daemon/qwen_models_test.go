package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func writeQwenSettings(t *testing.T, home, content string) {
	t.Helper()
	settingsDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The list form is what Qwen Code's own add-custom-model flow persists —
// verified against 0.24.6 on the deployment host.
func TestLoadQwenConfiguredModelsListForm(t *testing.T) {
	home := t.TempDir()
	writeQwenSettings(t, home, `{
  "modelProviders": {
    "openai": [
      {"id": "zai-org/GLM-5.3", "name": "zai-org/GLM-5.3", "baseUrl": "https://api.siliconflow.cn/v1"},
      {"id": "glm-5.3", "name": "GLM via SiliconFlow", "baseUrl": "https://api.siliconflow.cn/v1"},
      {"id": "glm-5.3", "name": "duplicate id", "baseUrl": "https://elsewhere.example/v1"},
      {"id": "qwen3.8-max", "name": "[ModelStudio Standard] qwen3.8-max", "baseUrl": "https://dashscope.aliyuncs.com/compatible-mode/v1"},
      {"id": "", "name": "no id", "baseUrl": "https://x.example"}
    ]
  },
  "model": {"name": "zai-org/GLM-5.3"}
}`)

	models, defaultID, err := loadQwenConfiguredModels(home)
	if err != nil {
		t.Fatal(err)
	}
	if defaultID != "zai-org/GLM-5.3" {
		t.Fatalf("defaultID = %q", defaultID)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	// Sorted by provider host then label: api.siliconflow.cn < dashscope.aliyuncs.com
	if models[0].ID != "glm-5.3" || models[0].Label != "GLM via SiliconFlow" || models[0].Provider != "api.siliconflow.cn" {
		t.Fatalf("first model = %+v", models[0])
	}
	// First occurrence wins for duplicate ids; empty ids are skipped.
	if models[1].ID != "zai-org/GLM-5.3" || models[1].Provider != "api.siliconflow.cn" {
		t.Fatalf("second model = %+v", models[1])
	}
	if models[2].ID != "qwen3.8-max" || models[2].Label != "[ModelStudio Standard] qwen3.8-max" || models[2].Provider != "dashscope.aliyuncs.com" {
		t.Fatalf("third model = %+v", models[2])
	}
}

// The object form is the hand-authored custom-provider shape from Qwen Code's
// own custom-provider documentation: a provider block with a nested models
// map, inheriting the provider-level baseUrl.
func TestLoadQwenConfiguredModelsProviderObjectForm(t *testing.T) {
	home := t.TempDir()
	writeQwenSettings(t, home, `{
  "modelProviders": {
    "my-llm": {
      "baseUrl": "https://my-llm.internal/v1",
      "models": {
        "my-llm/big": {"name": "Big Model"},
        "my-llm/small": {}
      }
    }
  }
}`)

	models, defaultID, err := loadQwenConfiguredModels(home)
	if err != nil {
		t.Fatal(err)
	}
	if defaultID != "" {
		t.Fatalf("defaultID = %q, want empty", defaultID)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "my-llm/big" || models[0].Label != "Big Model" || models[0].Provider != "my-llm.internal" {
		t.Fatalf("first model = %+v", models[0])
	}
	if models[1].ID != "my-llm/small" || models[1].Label != "my-llm/small" || models[1].Provider != "my-llm.internal" {
		t.Fatalf("second model = %+v", models[1])
	}
}

func TestLoadQwenConfiguredModelsMissingFile(t *testing.T) {
	models, defaultID, err := loadQwenConfiguredModels(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 || defaultID != "" {
		t.Fatalf("models = %+v, defaultID = %q", models, defaultID)
	}
}

func TestLoadQwenConfiguredModelsInvalidJSON(t *testing.T) {
	home := t.TempDir()
	writeQwenSettings(t, home, "{not json")
	if _, _, err := loadQwenConfiguredModels(home); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestMergeQwenModelCatalogs(t *testing.T) {
	catalog := []agent.Model{
		{ID: "qwen3.8-max-preview", Label: "Qwen3.8 Max Preview", Provider: "alibaba", Default: true},
		{ID: "qwen3.8-max", Label: "Qwen3.8 Max", Provider: "alibaba"},
	}
	custom := []agent.Model{
		{ID: "zai-org/GLM-5.3", Label: "zai-org/GLM-5.3", Provider: "api.siliconflow.cn"},
		{ID: "qwen3.8-max", Label: "hand-added duplicate of a builtin", Provider: "x.example"},
	}

	merged := mergeQwenModelCatalogs(catalog, custom, "zai-org/GLM-5.3")
	if len(merged) != 3 {
		t.Fatalf("merged = %+v", merged)
	}
	if merged[0].ID != "qwen3.8-max-preview" || merged[0].Default {
		t.Fatalf("static default must move to the settings pick: %+v", merged[0])
	}
	if merged[1].ID != "qwen3.8-max" || merged[1].Label != "Qwen3.8 Max" || merged[1].Provider != "alibaba" {
		t.Fatalf("catalog wins on id collision: %+v", merged[1])
	}
	if merged[2].ID != "zai-org/GLM-5.3" || !merged[2].Default {
		t.Fatalf("settings default should badge the custom model: %+v", merged[2])
	}

	// Settings default on a builtin id: the badge moves onto the static row.
	merged = mergeQwenModelCatalogs(catalog, custom, "qwen3.8-max")
	if merged[0].Default || !merged[1].Default {
		t.Fatalf("badge should follow the settings pick: %+v %+v", merged[0], merged[1])
	}

	// A stale settings default matching nothing leaves the catalog untouched.
	merged = mergeQwenModelCatalogs(catalog, custom, "deleted/model")
	if !merged[0].Default {
		t.Fatalf("catalog defaults must survive an unmatchable settings default: %+v", merged[0])
	}
	merged = mergeQwenModelCatalogs(catalog, custom, "")
	if !merged[0].Default {
		t.Fatalf("empty settings default keeps the catalog default: %+v", merged[0])
	}
}

// TestHandleModelList_QwenMergesUserConfiguredModels pins the picker path
// end to end: the daemon's model list for a qwen runtime must carry the
// user-configured custom models from Qwen Code's settings, with the dropdown's
// default badge on the model the CLI itself has selected.
func TestHandleModelList_QwenMergesUserConfiguredModels(t *testing.T) {
	fx := newModelListFixture(t)
	d := fx.daemon

	listModels = func(_ context.Context, provider string, _ agent.Command) (agent.Catalog, error) {
		if provider != "qwen" {
			t.Errorf("discovery provider = %q, want qwen", provider)
		}
		return agent.Catalog{Models: []agent.Model{
			{ID: "qwen3.8-max-preview", Label: "Qwen3.8 Max Preview", Provider: "alibaba", Default: true},
			{ID: "qwen3.8-max", Label: "Qwen3.8 Max", Provider: "alibaba"},
		}}, nil
	}

	home := t.TempDir()
	writeQwenSettings(t, home, `{
  "modelProviders": {
    "openai": [
      {"id": "zai-org/GLM-5.3", "name": "zai-org/GLM-5.3", "baseUrl": "https://api.siliconflow.cn/v1"}
    ]
  },
  "model": {"name": "zai-org/GLM-5.3"}
}`)
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}

	d.cfg.Agents = map[string]AgentEntry{"qwen": {Path: fakeExecutable(t, "qwen")}}
	d.runtimeIndex["rt-qwen"] = Runtime{ID: "rt-qwen", Provider: "qwen"}

	d.handleModelList(context.Background(), d.runtimeIndex["rt-qwen"], "req-qwen")

	_, _, _, report := fx.snapshot()
	if got := report["status"]; got != "completed" {
		t.Fatalf("report status = %v (report: %v)", got, report)
	}
	rawModels, err := json.Marshal(report["models"])
	if err != nil {
		t.Fatal(err)
	}
	var models []agent.Model
	if err := json.Unmarshal(rawModels, &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "qwen3.8-max-preview" || models[0].Default {
		t.Fatalf("builtin row must lose the badge to the settings pick: %+v", models[0])
	}
	if models[1].ID != "qwen3.8-max" {
		t.Fatalf("second row = %+v", models[1])
	}
	if models[2].ID != "zai-org/GLM-5.3" || models[2].Provider != "api.siliconflow.cn" || !models[2].Default {
		t.Fatalf("custom row = %+v", models[2])
	}
}
