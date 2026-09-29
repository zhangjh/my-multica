package daemon

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// qwenUserSettingsPath returns Qwen Code's user settings file — the same file
// a daemon-launched `qwen` resolves through its inherited HOME. Qwen Code (as
// of 0.24.6) accepts no config-directory override, so unlike CodeBuddy's
// $CODEBUDDY_CONFIG_DIR there is no candidate chain: one path, one file.
func qwenUserSettingsPath(home string) string {
	return filepath.Join(home, ".qwen", "settings.json")
}

// loadQwenConfiguredModels reads the custom models Qwen Code's own /model
// picker offers — everything in modelProviders, minus the built-ins — plus the
// id it currently has selected (model.name) as the default-model hint.
//
// Qwen Code has no `models list` subcommand to discover against, but the
// selectable set is exactly "built-in ids + modelProviders ids": a custom id
// only becomes usable once the CLI's picker has persisted the entry with the
// provider wiring (baseUrl, envKey) the id resolves through. Enumerating the
// settings file is therefore complete discovery — non-network, and in sync
// with what a daemon-launched `qwen --model <id>` on this host actually
// accepts. Only user-scope settings are read; the CLI's custom-model flow
// writes there.
//
// modelProviders is keyed by wire-format family ("openai" for the
// OpenAI-compatible models the picker's add-custom-model flow writes; a
// hand-authored provider uses its own key) and holds either that flat model
// list or a provider object with a nested `models` block — the shape Qwen
// Code's own custom-provider documentation uses. Both are parsed. Duplicate
// ids collapse to the first entry: the id is what the dropdown persists and
// what --model sends, so it can only be offered once.
func loadQwenConfiguredModels(home string) ([]agent.Model, string, error) {
	raw, err := os.ReadFile(qwenUserSettingsPath(home))
	if os.IsNotExist(err) {
		return []agent.Model{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		ModelProviders map[string]json.RawMessage `json:"modelProviders"`
		Model          struct {
			Name string `json:"name"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", err
	}

	models := make([]agent.Model, 0)
	seen := make(map[string]bool)
	providerKeys := make([]string, 0, len(doc.ModelProviders))
	for key := range doc.ModelProviders {
		providerKeys = append(providerKeys, key)
	}
	sort.Strings(providerKeys)
	for _, key := range providerKeys {
		for _, m := range parseQwenProviderModels(key, doc.ModelProviders[key]) {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			models = append(models, m)
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].Label < models[j].Label
	})
	return models, strings.TrimSpace(doc.Model.Name), nil
}

// parseQwenProviderModels enumerates one modelProviders entry.
func parseQwenProviderModels(providerKey string, raw json.RawMessage) []agent.Model {
	toModel := func(id, name, baseURL string) (agent.Model, bool) {
		id = strings.TrimSpace(id)
		if id == "" {
			return agent.Model{}, false
		}
		label := strings.TrimSpace(name)
		if label == "" {
			label = id
		}
		return agent.Model{
			ID:       id,
			Label:    label,
			Provider: qwenModelGroup(providerKey, baseURL),
		}, true
	}

	// Shape 1 — the flat custom-model list the CLI's picker writes:
	//   "openai": [ { "id": ..., "name": ..., "baseUrl": ... }, ... ]
	var list []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]agent.Model, 0, len(list))
		for _, entry := range list {
			if m, ok := toModel(entry.ID, entry.Name, entry.BaseURL); ok {
				out = append(out, m)
			}
		}
		return out
	}

	// Shape 2 — a provider object with a nested models block, the hand-authored
	// custom-provider form; its models inherit the provider-level baseUrl:
	//   "my-llm": { "baseUrl": ..., "models": { "m-1": { "name": ... } } }
	var obj struct {
		BaseURL string          `json:"baseUrl"`
		Models  json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || len(obj.Models) == 0 {
		return nil
	}
	var nestedList []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(obj.Models, &nestedList); err == nil {
		out := make([]agent.Model, 0, len(nestedList))
		for _, entry := range nestedList {
			if m, ok := toModel(entry.ID, entry.Name, obj.BaseURL); ok {
				out = append(out, m)
			}
		}
		return out
	}
	var nestedMap map[string]struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(obj.Models, &nestedMap); err == nil {
		ids := make([]string, 0, len(nestedMap))
		for id := range nestedMap {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := make([]agent.Model, 0, len(ids))
		for _, id := range ids {
			if m, ok := toModel(id, nestedMap[id].Name, obj.BaseURL); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// qwenModelGroup derives the dropdown's group header for a custom model. The
// settings file groups custom models only by wire-format family, which would
// headline GLM, Kimi, and Qwen rows all as "openai"; the host a model actually
// talks to carries the information the user picked it by. Entries without a
// baseUrl fall back to the settings provider key, then "custom".
func qwenModelGroup(providerKey, baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		if !strings.Contains(baseURL, "://") {
			baseURL = "https://" + baseURL
		}
		if parsed, err := url.Parse(baseURL); err == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
	}
	if key := strings.TrimSpace(providerKey); key != "" {
		return key
	}
	return "custom"
}

// mergeQwenModelCatalogs overlays the daemon-discovered custom models on the
// built-in static catalog:
//   - the catalog wins on id collisions, so a custom entry that merely
//     redeclares a built-in id keeps the labeled, priced row;
//   - customs keep their own deterministic order, appended after the
//     built-ins the static catalog already returns;
//   - the Default badge — display-only, never consulted at launch — moves to
//     the id Qwen Code itself has selected (model.name) whenever that id is
//     present in either list, so the preselected row is the model a `qwen`
//     with no --model would actually run. A settings default that matches
//     nothing (a stale model.name after its entry was deleted) leaves the
//     catalog's own defaults untouched.
func mergeQwenModelCatalogs(catalog, custom []agent.Model, settingsDefaultID string) []agent.Model {
	present := make(map[string]bool, len(catalog)+len(custom))
	for _, m := range catalog {
		present[m.ID] = true
	}
	for _, m := range custom {
		present[m.ID] = true
	}

	settingsDefaultID = strings.TrimSpace(settingsDefaultID)
	defaultID := ""
	if settingsDefaultID != "" && present[settingsDefaultID] {
		defaultID = settingsDefaultID
	}

	out := make([]agent.Model, 0, len(catalog)+len(custom))
	for _, m := range catalog {
		if defaultID != "" {
			m.Default = m.ID == defaultID
		}
		out = append(out, m)
	}
	inCatalog := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		inCatalog[m.ID] = true
	}
	for _, m := range custom {
		if inCatalog[m.ID] {
			continue
		}
		inCatalog[m.ID] = true
		m.Default = m.ID == defaultID
		out = append(out, m)
	}
	return out
}
