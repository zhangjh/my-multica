package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newAgentExportTestCmd builds a standalone cobra.Command carrying the same
// flags runAgentExport reads (via the shared registrar), plus the persistent
// --profile flag the API-client resolver needs.
func newAgentExportTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "export"}
	registerAgentExportFlags(c)
	c.Flags().String("profile", "", "")
	return c
}

// newAgentImportTestCmd builds a standalone cobra.Command carrying the same
// flags runAgentImport reads (via the shared registrar), plus the persistent
// --profile flag the API-client resolver needs.
func newAgentImportTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "import"}
	registerAgentImportFlags(c)
	c.Flags().String("profile", "", "")
	return c
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// exportMockServer serves the endpoints runAgentExport reads: agent list,
// runtime list, per-agent /env, and full skill definitions.
func exportMockServer(t *testing.T, agents []map[string]any, envByAgent map[string]map[string]any, skillsByID map[string]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/api/agents":
			_ = json.NewEncoder(w).Encode(agents)
		case r.Method == http.MethodGet && path == "/api/runtimes":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "rt-1", "name": "workstation", "custom_name": nil, "runtime_mode": "codex", "provider": "codex"},
			})
		case r.Method == http.MethodGet && path == "/api/skills/skill-1":
			if r.URL.Query().Get("include") != "content" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(skillsByID["skill-1"])
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/agents/") && strings.HasSuffix(path, "/env"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/agents/"), "/env")
			env, ok := envByAgent[id]
			if !ok {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "forbidden"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": id, "custom_env": env})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func exportableAgent() map[string]any {
	return map[string]any{
		"id":           "agent-1",
		"name":         "Dev Helper",
		"runtime_id":   "rt-1",
		"description":  "Build helper",
		"instructions": "Do the thing.",
		"conversation_starters": []any{
			map[string]any{"label": "Hi", "prompt": "Hello there"},
		},
		"custom_args":          []any{"--foo", "--bar"},
		"max_concurrent_tasks": 4,
		"model":                "claude-sonnet-4-6",
		"thinking_level":       "high",
		"service_tier":         "priority",
		"permission_mode":      "public_to",
		"invocation_targets":   []any{map[string]any{"target_type": "workspace"}},
		"skills":               []any{map[string]any{"id": "skill-1", "name": "Code Review"}},
		"mcp_config":           map[string]any{"servers": []any{}},
		"mcp_config_redacted":  false,
		"has_custom_env":       true,
	}
}

func exportSkill() map[string]any {
	return map[string]any{
		"id":          "skill-1",
		"name":        "Code Review",
		"description": "Review code well",
		"content":     "# Code Review\n\nBe thorough.",
		"config":      map[string]any{},
		"files": []any{
			map[string]any{"id": "f1", "skill_id": "skill-1", "path": "tools.py", "content": "print(1)"},
		},
	}
}

func readExportFile(t *testing.T, path string) agentExportFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var export agentExportFile
	if err := json.Unmarshal(data, &export); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	return export
}

func TestAgentExportWritesFullPortableFields(t *testing.T) {
	srv := exportMockServer(t, []map[string]any{exportableAgent()},
		map[string]map[string]any{"agent-1": {"API_KEY": "sekrit", "HOST": "example"}},
		map[string]map[string]any{"skill-1": exportSkill()})
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	out := filepath.Join(t.TempDir(), "agents.json")
	cmd := newAgentExportTestCmd()
	_ = cmd.Flags().Set("output", out)

	if err := runAgentExport(cmd, nil); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	export := readExportFile(t, out)
	if export.Version != 1 || export.Kind != "multica-agent-export" {
		t.Errorf("version/kind = %d/%q, want 1/multica-agent-export", export.Version, export.Kind)
	}
	if export.Source.ServerURL == "" || export.Source.WorkspaceID != "ws-1" {
		t.Errorf("source = %+v", export.Source)
	}
	if len(export.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(export.Agents))
	}
	a := export.Agents[0]
	if a.Name != "Dev Helper" || a.Description != "Build helper" || a.Instructions != "Do the thing." {
		t.Errorf("plain fields = %+v", a)
	}
	if a.Model != "claude-sonnet-4-6" || a.ThinkingLevel != "high" || a.ServiceTier != "priority" {
		t.Errorf("runtime-specific fields = %+v", a)
	}
	if a.RuntimeName != "workstation" || a.RuntimeSourceID != "rt-1" || a.RuntimeProvider != "codex" {
		t.Errorf("runtime = %s/%s/%s", a.RuntimeName, a.RuntimeSourceID, a.RuntimeProvider)
	}
	if a.PermissionMode != "public_to" {
		t.Errorf("permission_mode = %q", a.PermissionMode)
	}
	if a.MaxConcurrentTasks == nil || *a.MaxConcurrentTasks != 4 {
		t.Errorf("max_concurrent_tasks = %v, want 4", a.MaxConcurrentTasks)
	}
	if !reflect.DeepEqual(a.CustomArgs, []any{"--foo", "--bar"}) {
		t.Errorf("custom_args = %v", a.CustomArgs)
	}
	if !reflect.DeepEqual(a.ConversationStarters, []any{map[string]any{"label": "Hi", "prompt": "Hello there"}}) {
		t.Errorf("conversation_starters = %v", a.ConversationStarters)
	}
	// Secrets come from the /env endpoint, not the redacted agent payload.
	if a.CustomEnv["API_KEY"] != "sekrit" || a.CustomEnv["HOST"] != "example" {
		t.Errorf("custom_env = %v", a.CustomEnv)
	}
	if len(a.McpConfig) == 0 {
		t.Error("mcp_config should be exported when not redacted")
	}
	var mc map[string]any
	if err := json.Unmarshal(a.McpConfig, &mc); err != nil || mc["servers"] == nil {
		t.Errorf("mcp_config = %s (err %v)", a.McpConfig, err)
	}
	if !reflect.DeepEqual(a.SkillNames, []string{"Code Review"}) {
		t.Errorf("skill_names = %v", a.SkillNames)
	}
	if len(a.Notes) != 0 {
		t.Errorf("notes = %v, want none", a.Notes)
	}

	// Skills carry their full definitions so import can recreate them.
	if len(export.Skills) != 1 {
		t.Fatalf("skills = %d, want 1", len(export.Skills))
	}
	sk := export.Skills[0]
	if sk.SourceID != "skill-1" || sk.Name != "Code Review" || sk.Description != "Review code well" {
		t.Errorf("skill = %+v", sk)
	}
	if sk.Content != "# Code Review\n\nBe thorough." {
		t.Errorf("skill content = %q", sk.Content)
	}
	if len(sk.Files) != 1 || sk.Files[0].Path != "tools.py" || sk.Files[0].Content != "print(1)" {
		t.Errorf("skill files = %+v", sk.Files)
	}

	// The file may carry credentials; it must be created private.
	if fi, err := os.Stat(out); err != nil {
		t.Fatalf("stat export: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("export file mode = %o, want 600", perm)
	}
}

// A redacted mcp_config travels through the export as an empty field plus a
// notice; it is never turned into a real, unusable value.
func TestAgentExportRecordsRedactedMcpConfig(t *testing.T) {
	agent := exportableAgent()
	agent["mcp_config_redacted"] = true
	agent["mcp_config"] = nil

	srv := exportMockServer(t, []map[string]any{agent},
		map[string]map[string]any{"agent-1": {"API_KEY": "sekrit"}},
		map[string]map[string]any{"skill-1": exportSkill()})
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	out := filepath.Join(t.TempDir(), "agents.json")
	cmd := newAgentExportTestCmd()
	_ = cmd.Flags().Set("output", out)
	if err := runAgentExport(cmd, nil); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	a := readExportFile(t, out).Agents[0]
	if len(a.McpConfig) != 0 {
		t.Errorf("redacted mcp_config must not be exported, got %s", a.McpConfig)
	}
	if !strings.Contains(strings.Join(a.Notes, "\n"), "mcp_config") {
		t.Errorf("expected a note about mcp_config being redacted, got %v", a.Notes)
	}
}

// A 403 on the audited /env endpoint is a per-agent warning, not an abort.
func TestAgentExportRecordsEnvReadFailure(t *testing.T) {
	srv := exportMockServer(t, []map[string]any{exportableAgent()},
		map[string]map[string]any{}, // no env -> handler 403s
		map[string]map[string]any{"skill-1": exportSkill()})
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	out := filepath.Join(t.TempDir(), "agents.json")
	cmd := newAgentExportTestCmd()
	_ = cmd.Flags().Set("output", out)
	if err := runAgentExport(cmd, nil); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	a := readExportFile(t, out).Agents[0]
	if a.CustomEnv != nil {
		t.Errorf("custom_env must be omitted after a read failure, got %v", a.CustomEnv)
	}
	if !strings.Contains(strings.Join(a.Notes, "\n"), "custom_env") {
		t.Errorf("expected a note about custom_env, got %v", a.Notes)
	}
}

func TestAgentExportSkipsSystemAndControlsArchived(t *testing.T) {
	agents := []map[string]any{
		exportableAgent(),
		func() map[string]any {
			a := exportableAgent()
			a["id"] = "agent-archived"
			a["name"] = "Old"
			a["archived_at"] = "2026-01-01T00:00:00Z"
			return a
		}(),
		func() map[string]any {
			a := exportableAgent()
			a["id"] = "agent-system"
			a["name"] = "Mika"
			a["system_key"] = "mika"
			return a
		}(),
	}

	srv := exportMockServer(t, agents,
		map[string]map[string]any{"agent-1": {"API_KEY": "sekrit"}},
		map[string]map[string]any{"skill-1": exportSkill()})
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	// Default: archived agents stay out; system agents never appear.
	out := filepath.Join(t.TempDir(), "d.json")
	cmd := newAgentExportTestCmd()
	_ = cmd.Flags().Set("output", out)
	if err := runAgentExport(cmd, nil); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}
	export := readExportFile(t, out)
	if len(export.Agents) != 1 || export.Agents[0].Name != "Dev Helper" {
		t.Errorf("without --include-archived agents = %+v", export.Agents)
	}

	// --include-archived brings the archived agent back, flagged as such.
	out2 := filepath.Join(t.TempDir(), "d2.json")
	cmd2 := newAgentExportTestCmd()
	_ = cmd2.Flags().Set("output", out2)
	_ = cmd2.Flags().Set("include-archived", "true")
	if err := runAgentExport(cmd2, nil); err != nil {
		t.Fatalf("runAgentExport(include-archived): %v", err)
	}
	export2 := readExportFile(t, out2)
	if len(export2.Agents) != 2 {
		t.Fatalf("with --include-archived agents = %+v", export2.Agents)
	}
	if !export2.Agents[1].Archived {
		t.Error("archived agent must be flagged Archived")
	}
	for _, a := range export2.Agents {
		if a.Name == "Mika" {
			t.Error("system agent must never be exported")
		}
	}
}

// --agent filters the export to the named source agents.
func TestAgentExportFiltersByAgentID(t *testing.T) {
	other := exportableAgent()
	other["id"] = "agent-other"
	other["name"] = "Other"

	srv := exportMockServer(t, []map[string]any{exportableAgent(), other},
		map[string]map[string]any{"agent-1": {"API_KEY": "sekrit"}},
		map[string]map[string]any{"skill-1": exportSkill()})
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	out := filepath.Join(t.TempDir(), "a.json")
	cmd := newAgentExportTestCmd()
	_ = cmd.Flags().Set("output", out)
	_ = cmd.Flags().Set("agent", "agent-other")
	if err := runAgentExport(cmd, nil); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}
	export := readExportFile(t, out)
	if len(export.Agents) != 1 || export.Agents[0].Name != "Other" {
		t.Errorf("filtered agents = %+v", export.Agents)
	}
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

// importMockServer records every agent/skill it creates and serves a fixed
// runtime set so callers can exercise name matching, conflict handling, and
// skill recreation.
type importMockServer struct {
	t              *testing.T
	srv            *httptest.Server
	createdAgents  []map[string]any
	createdSkills  []map[string]any
	archiveIDs     []string
	conflictNames  map[string]bool
	existingSkills []map[string]any
}

func newImportMockServer(t *testing.T, existingSkills []map[string]any, conflictNames map[string]bool) *importMockServer {
	t.Helper()
	m := &importMockServer{t: t, conflictNames: conflictNames, existingSkills: existingSkills}
	agentSeq := 0
	skillSeq := 0
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/api/runtimes":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "rt-1", "name": "workstation", "custom_name": nil},
			})
		case r.Method == http.MethodGet && path == "/api/skills":
			_ = json.NewEncoder(w).Encode(m.existingSkills)
		case r.Method == http.MethodPost && path == "/api/skills":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode skill create: %v", err)
			}
			m.createdSkills = append(m.createdSkills, body)
			skillSeq++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "skill-new-" + itoa(skillSeq), "name": body["name"]})
		case r.Method == http.MethodPost && path == "/api/agents":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode agent create: %v", err)
			}
			name, _ := body["name"].(string)
			if m.conflictNames[name] {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "an agent named \"" + name + "\" already exists in this workspace"})
				return
			}
			m.createdAgents = append(m.createdAgents, body)
			agentSeq++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "agent-new-" + itoa(agentSeq), "name": name})
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/agents/") && strings.HasSuffix(path, "/archive"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/agents/"), "/archive")
			m.archiveIDs = append(m.archiveIDs, id)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "archived_at": "2026-09-01T00:00:00Z"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return m
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}

func writeImportFile(t *testing.T, export agentExportFile) string {
	t.Helper()
	data, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}
	return path
}

// importFixture builds a representative agent export plus its skill.
func importFixture() (agentExportFile, agentExportEntry) {
	entry := agentExportEntry{
		SourceID:       "agent-1",
		Name:           "Dev Helper",
		RuntimeName:    "workstation",
		Description:    "Build helper",
		Instructions:   "Do the thing.",
		Model:          "claude-sonnet-4-6",
		ThinkingLevel:  "high",
		ServiceTier:    "priority",
		PermissionMode: "public_to",
		CustomEnv:      map[string]string{"API_KEY": "sekrit"},
		McpConfig:      json.RawMessage(`{"servers":[]}`),
		SkillNames:     []string{"Code Review"},
		ConversationStarters: []any{
			map[string]any{"label": "Hi", "prompt": "Hello there"},
		},
	}
	max := int32(4)
	entry.MaxConcurrentTasks = &max
	export := agentExportFile{
		Version:    agentExportVersion,
		Kind:       agentExportKind,
		ExportedAt: "2026-09-01T00:00:00Z",
		Skills: []agentExportSkill{{
			SourceID:    "skill-1",
			Name:        "Code Review",
			Description: "Review code well",
			Content:     "# Code Review\n\nBe thorough.",
		}},
		Agents: []agentExportEntry{entry},
	}
	return export, entry
}

func TestAgentImportReusesExistingSkillAndPreservesFields(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, []map[string]any{{"id": "skill-t", "name": "Code Review"}}, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}

	if len(m.createdAgents) != 1 {
		t.Fatalf("created agents = %d, want 1", len(m.createdAgents))
	}
	body := m.createdAgents[0]
	if body["name"] != "Dev Helper" {
		t.Errorf("name = %v", body["name"])
	}
	if body["runtime_id"] != "rt-1" {
		t.Errorf("runtime_id = %v, want rt-1 (matched by display name)", body["runtime_id"])
	}
	if body["description"] != "Build helper" || body["instructions"] != "Do the thing." {
		t.Errorf("text fields = %v", body)
	}
	if body["model"] != "claude-sonnet-4-6" || body["thinking_level"] != "high" || body["service_tier"] != "priority" {
		t.Errorf("runtime-specific = %v", body)
	}
	if body["max_concurrent_tasks"] != float64(4) {
		t.Errorf("max_concurrent_tasks = %v", body["max_concurrent_tasks"])
	}
	// custom_env / mcp_config round-trip through the file.
	if !reflect.DeepEqual(body["custom_env"], map[string]any{"API_KEY": "sekrit"}) {
		t.Errorf("custom_env = %v", body["custom_env"])
	}
	if !reflect.DeepEqual(body["mcp_config"], map[string]any{"servers": []any{}}) {
		t.Errorf("mcp_config = %v", body["mcp_config"])
	}
	// Permission mode only: the invocation allow-list is never migrated.
	if body["permission_mode"] != "public_to" {
		t.Errorf("permission_mode = %v", body["permission_mode"])
	}
	if _, ok := body["invocation_targets"]; ok {
		t.Errorf("invocation_targets must not be migrated, got %v", body["invocation_targets"])
	}
	// The existing target skill is reused; no skill is recreated.
	if !reflect.DeepEqual(body["skill_ids"], []any{"skill-t"}) {
		t.Errorf("skill_ids = %v, want [skill-t]", body["skill_ids"])
	}
	if len(m.createdSkills) != 0 {
		t.Errorf("skills created = %+v, want none (reuse)", m.createdSkills)
	}
}

func TestAgentImportCreatesMissingSkill(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}

	if len(m.createdSkills) != 1 {
		t.Fatalf("skills created = %d, want 1", len(m.createdSkills))
	}
	skill := m.createdSkills[0]
	if skill["name"] != "Code Review" {
		t.Errorf("skill name = %v", skill["name"])
	}
	if skill["content"] != "# Code Review\n\nBe thorough." {
		t.Errorf("skill content = %v", skill["content"])
	}
	if !reflect.DeepEqual(m.createdAgents[0]["skill_ids"], []any{"skill-new-1"}) {
		t.Errorf("skill_ids = %v, want [skill-new-1]", m.createdAgents[0]["skill_ids"])
	}
}

func TestAgentImportNoSkills(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	_ = cmd.Flags().Set("no-skills", "true")
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if _, ok := m.createdAgents[0]["skill_ids"]; ok {
		t.Errorf("skill_ids must be omitted with --no-skills")
	}
	if len(m.createdSkills) != 0 {
		t.Errorf("skills must not be created with --no-skills")
	}
}

func TestAgentImportConflictSkip(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, nil, map[string]bool{"Dev Helper": true})
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	_ = cmd.Flags().Set("on-conflict", "skip")
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(m.createdAgents) != 0 {
		t.Errorf("no agent may be created on skip")
	}
}

func TestAgentImportConflictRename(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, nil, map[string]bool{"Dev Helper": true, "Dev Helper (imported)": false})
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	_ = cmd.Flags().Set("on-conflict", "rename")
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(m.createdAgents) != 1 {
		t.Fatalf("created agents = %d, want 1", len(m.createdAgents))
	}
	if m.createdAgents[0]["name"] != "Dev Helper (imported)" {
		t.Errorf("renamed agent name = %v", m.createdAgents[0]["name"])
	}
}

func TestAgentImportConflictFail(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, nil, map[string]bool{"Dev Helper": true})
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	_ = cmd.Flags().Set("on-conflict", "fail")
	err := runAgentImport(cmd, nil)
	if err == nil {
		t.Fatal("expected an error on --on-conflict fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want a conflict message", err.Error())
	}
}

func TestAgentImportUnresolvedRuntime(t *testing.T) {
	export, _ := importFixture()
	export.Agents[0].RuntimeName = "elsewhere-machine"
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	err := runAgentImport(cmd, nil)
	if err == nil {
		t.Fatal("expected an error for an unmappable runtime")
	}
	if !strings.Contains(err.Error(), "--default-runtime") {
		t.Errorf("error = %q, want a --default-runtime hint", err.Error())
	}
	if len(m.createdAgents) != 0 {
		t.Errorf("no agent may be created when its runtime is unresolved")
	}
}

func TestAgentImportDefaultRuntimeFallback(t *testing.T) {
	export, _ := importFixture()
	export.Agents[0].RuntimeName = "elsewhere-machine"
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	_ = cmd.Flags().Set("default-runtime", "rt-1")
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if m.createdAgents[0]["runtime_id"] != "rt-1" {
		t.Errorf("runtime_id = %v, want the --default-runtime", m.createdAgents[0]["runtime_id"])
	}
}

func TestAgentImportDryRunWritesNothing(t *testing.T) {
	export, _ := importFixture()
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	_ = cmd.Flags().Set("dry-run", "true")
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(m.createdAgents) != 0 || len(m.createdSkills) != 0 {
		t.Errorf("dry-run must not create anything (agents=%d skills=%d)", len(m.createdAgents), len(m.createdSkills))
	}
}

func TestAgentImportRestoresArchivedAgent(t *testing.T) {
	export, _ := importFixture()
	export.Agents[0].Archived = true
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, export))
	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(m.createdAgents) != 1 {
		t.Fatalf("created agents = %d", len(m.createdAgents))
	}
	if !reflect.DeepEqual(m.archiveIDs, []string{"agent-new-1"}) {
		t.Errorf("archiveIDs = %v, want the created agent id", m.archiveIDs)
	}
}

func TestAgentImportValidatesFileHeader(t *testing.T) {
	m := newImportMockServer(t, nil, nil)
	defer m.srv.Close()
	setCopyTestEnv(t, m.srv.URL)

	bad := agentExportFile{Version: agentExportVersion, Kind: "something-else", Agents: []agentExportEntry{{Name: "X"}}}
	cmd := newAgentImportTestCmd()
	_ = cmd.Flags().Set("file", writeImportFile(t, bad))
	if err := runAgentImport(cmd, nil); err == nil || !strings.Contains(err.Error(), "not a multica-agent-export") {
		t.Errorf("kind validation err = %v", err)
	}

	oldVersion := agentExportFile{Version: 99, Kind: agentExportKind, Agents: []agentExportEntry{{Name: "X"}}}
	cmd2 := newAgentImportTestCmd()
	_ = cmd2.Flags().Set("file", writeImportFile(t, oldVersion))
	if err := runAgentImport(cmd2, nil); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("version validation err = %v", err)
	}
}

func TestAgentImportRequiresFileFlag(t *testing.T) {
	cmd := newAgentImportTestCmd()
	if err := runAgentImport(cmd, nil); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Errorf("err = %v, want a --file error", err)
	}
}
