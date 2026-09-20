package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// importExportRequest builds a JSON request against the import endpoint as the
// owner of the test workspace.
func importExportRequest(method, path string, body any) *http.Request {
	var req *http.Request
	if s, ok := body.(string); ok {
		req = testutil.JSONRequest(method, path, s)
	} else {
		req = testutil.JSONRequest(method, path, body)
	}
	return testutil.WithHeaders(req, "X-User-ID", testUserID, "X-Workspace-ID", testWorkspaceID)
}

func marshalExportFile(t *testing.T, exp agentExport) []byte {
	t.Helper()
	b, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal export file: %v", err)
	}
	return b
}

func TestAgentExportImport_RestrictedToOwnerAdmin(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	memberID := createPermissionTestMember(t, "agent-export-nonadmin@example.com")

	exportReq := newRequest("GET", "/api/agents/export", nil)
	exportReq.Header.Set("X-User-ID", memberID)
	testutil.Call(t, testHandler.ExportAgents, exportReq).Want(http.StatusForbidden)

	importReq := newRequest("POST", "/api/agents/import", map[string]any{
		"kind": agentExportKind, "version": agentExportVersion,
		"agents": []any{map[string]any{"name": "nope"}},
	})
	importReq.Header.Set("X-User-ID", memberID)
	testutil.Call(t, testHandler.ImportAgents, importReq).Want(http.StatusForbidden)
}

func TestAgentExport_IncludesSecretsSkillsAndRuntime(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := handlerTestRuntimeID(t)
	skillID := insertHandlerTestSkill(t, "export-me-skill", "export-me-skill-content")
	var skillName string
	dbfx.QueryRow(t, `SELECT name FROM skill WHERE id = $1`, skillID).Scan(&skillName)

	agentID := dbfx.Agent(t, "export-me-"+t.Name(), runtimeID, testutil.Cols{
		"custom_env":      testutil.Raw(`'{"SECRET_KEY":"hunter2"}'::jsonb`),
		"mcp_config":      testutil.Raw(`'{"mcpServers":{"gh":{"url":"https://api.github.com","token":"sek"}}}'::jsonb`),
		"custom_args":     testutil.Raw(`'["--verbose"]'::jsonb`),
		"permission_mode": "public_to",
		"visibility":      "workspace",
		"model":           "gpt-5",
	})
	dbfx.Exec(t, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, skillID)

	testutil.Call(t, testHandler.ExportAgents, newRequest("GET", "/api/agents/export", nil)).
		Want(http.StatusOK).
		JSON(&struct {
			Version int    `json:"version"`
			Kind    string `json:"kind"`
		}{})
	rec := testutil.Call(t, testHandler.ExportAgents, newRequest("GET", "/api/agents/export", nil))
	var export agentExport
	rec.JSON(&export)

	if export.Kind != agentExportKind {
		t.Errorf("kind = %q, want %q", export.Kind, agentExportKind)
	}
	if export.Version != agentExportVersion {
		t.Errorf("version = %d, want %d", export.Version, agentExportVersion)
	}

	var found *agentExportEntry
	for i := range export.Agents {
		if export.Agents[i].SourceID == agentID {
			found = &export.Agents[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("exported file does not contain agent %s", agentID)
	}
	if got := found.CustomEnv["SECRET_KEY"]; got != "hunter2" {
		t.Errorf("custom_env SECRET_KEY = %q, want hunter2", got)
	}
	if !bytes.Contains(found.McpConfig, []byte("sek")) {
		t.Errorf("mcp_config did not carry plaintext token: %s", found.McpConfig)
	}
	if found.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", found.Model)
	}
	if found.RuntimeName != "Handler Test Runtime" {
		t.Errorf("runtime_name = %q, want Handler Test Runtime", found.RuntimeName)
	}
	if len(found.SkillNames) != 1 || found.SkillNames[0] != skillName {
		t.Errorf("skill_names = %v, want [%s]", found.SkillNames, skillName)
	}

	var def *agentExportSkill
	for i := range export.Skills {
		if export.Skills[i].Name == skillName {
			def = &export.Skills[i]
			break
		}
	}
	if def == nil {
		t.Fatalf("exported file does not contain skill definition %q", skillName)
	}
	if def.Content != "export-me-skill-content" {
		t.Errorf("skill content = %q, want full definition", def.Content)
	}
}

func TestAgentImport_RejectsInvalidFiles(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	valid := agentExport{Version: agentExportVersion, Kind: agentExportKind, Agents: []agentExportEntry{{Name: "x"}}}
	cases := []struct {
		name string
		body any
	}{
		{"wrong kind", marshalExportFile(t, agentExport{Version: agentExportVersion, Kind: "something-else", Agents: valid.Agents})},
		{"bad version", marshalExportFile(t, agentExport{Version: 99, Kind: agentExportKind, Agents: valid.Agents})},
		{"no agents", marshalExportFile(t, agentExport{Version: agentExportVersion, Kind: agentExportKind})},
		{"garbage", "definitely not an export file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, testHandler.ImportAgents,
				importExportRequest("POST", "/api/agents/import?on_conflict=skip", tc.body)).
				Want(http.StatusBadRequest)
		})
	}
}

func TestAgentImport_RestoresAgentSecretsAndSkills(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	skillName := "resurrected-skill-" + t.Name()
	agentName := "resurrected-agent" + t.Name()

	exp := agentExport{
		Version: agentExportVersion,
		Kind:    agentExportKind,
		Skills: []agentExportSkill{
			{Name: skillName, Description: "fixture", Content: "SKILL BODY", Files: []agentExportSkillFile{{Path: "guide.md", Content: "# Guide"}}},
		},
		Agents: []agentExportEntry{{
			Name:           agentName,
			RuntimeName:    "Handler Test Runtime",
			Description:    "restored from export",
			Instructions:   "do the thing",
			PermissionMode: "public_to",
			Model:          "gpt-5",
			ThinkingLevel:  "high",
			CustomEnv:      map[string]string{"SECRET": "hunter2"},
			McpConfig:      json.RawMessage(`{"mcpServers":{"x":{"url":"https://x"}}}`),
			SkillNames:     []string{skillName},
		}},
	}

	ctx := context.Background()
	first := importExportRequest("POST", "/api/agents/import?on_conflict=skip", marshalExportFile(t, exp))
	var report agentImportReport
	testutil.Call(t, testHandler.ImportAgents, first).Want(http.StatusOK).JSON(&report)
	if len(report.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(report.Results))
	}
	res := report.Results[0]
	if res.Status != "created" {
		t.Fatalf("status = %s (error=%s), want created", res.Status, res.Error)
	}
	if res.ID == "" {
		t.Fatal("created agent has no id")
	}
	if len(res.Notes) == 0 || !slices.ContainsFunc(res.Notes, func(n string) bool {
		return strings.Contains(n, "invocation allow-list not migrated")
	}) {
		t.Errorf("public_to agent notes = %v, want allow-list migration note", res.Notes)
	}

	stored, err := testHandler.Queries.GetAgent(ctx, parseUUID(res.ID))
	if err != nil {
		t.Fatalf("load imported agent: %v", err)
	}
	if got := unmarshalCustomEnv(stored)["SECRET"]; got != "hunter2" {
		t.Errorf("custom_env SECRET = %q, want hunter2", got)
	}
	if !bytes.Contains(stored.McpConfig, []byte("https://x")) {
		t.Errorf("mcp_config not restored: %s", stored.McpConfig)
	}
	if !stored.Model.Valid || stored.Model.String != "gpt-5" {
		t.Errorf("model = %v, want gpt-5", stored.Model)
	}
	if !stored.ThinkingLevel.Valid || stored.ThinkingLevel.String != "high" {
		t.Errorf("thinking_level = %v, want high", stored.ThinkingLevel)
	}

	skill, err := testHandler.Queries.GetSkillByWorkspaceAndName(ctx, db.GetSkillByWorkspaceAndNameParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		Name:        skillName,
	})
	if err != nil {
		t.Fatalf("load imported skill: %v", err)
	}
	if skill.Content != "SKILL BODY" {
		t.Errorf("skill content = %q, want SKILL BODY", skill.Content)
	}
	files, err := testHandler.Queries.ListSkillFiles(ctx, skill.ID)
	if err != nil || len(files) != 1 || files[0].Path != "guide.md" || files[0].Content != "# Guide" {
		t.Errorf("skill files = %+v (err=%v), want one guide.md", files, err)
	}
	summaries, err := testHandler.Queries.ListAgentSkillSummaries(ctx, parseUUID(res.ID))
	if err != nil || len(summaries) != 1 || summaries[0].Name != skillName {
		t.Errorf("bound skills = %+v (err=%v), want [%s]", summaries, err, skillName)
	}

	// A second import reuses the existing skill instead of duplicating it.
	second := exp
	second.Agents[0].Name = agentName + "-2"
	var report2 agentImportReport
	secondReq := importExportRequest("POST", "/api/agents/import?on_conflict=skip", marshalExportFile(t, second))
	testutil.Call(t, testHandler.ImportAgents, secondReq).Want(http.StatusOK).JSON(&report2)
	if len(report2.Results) != 1 || report2.Results[0].Status != "created" {
		t.Fatalf("second import results = %+v, want created", report2.Results)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM skill WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, skillName); got != 1 {
		t.Errorf("skill count = %d, want 1 (reused, not duplicated)", got)
	}

	skillIDStr := uuidToString(skill.ID)
	dbfx.Cleanup(t, `DELETE FROM skill WHERE id = $1`, skillIDStr)
	dbfx.Cleanup(t, `DELETE FROM agent WHERE id = $1`, report2.Results[0].ID)
	dbfx.Cleanup(t, `DELETE FROM agent WHERE id = $1`, res.ID)
	dbfx.Cleanup(t, `DELETE FROM agent_skill WHERE agent_id = $1`, report2.Results[0].ID)
	dbfx.Cleanup(t, `DELETE FROM agent_skill WHERE agent_id = $1`, res.ID)
}

func TestAgentImport_ConflictModes(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := "conflict-agent" + t.Name()
	dbfx.Agent(t, name, handlerTestRuntimeID(t))

	exp := agentExport{
		Version: agentExportVersion,
		Kind:    agentExportKind,
		Agents:  []agentExportEntry{{Name: name, RuntimeName: "Handler Test Runtime"}},
	}

	t.Run("fail_returns_409", func(t *testing.T) {
		var report agentImportReport
		testutil.Call(t, testHandler.ImportAgents,
			importExportRequest("POST", "/api/agents/import?on_conflict=fail", marshalExportFile(t, exp))).
			Want(http.StatusConflict).JSON(&report)
		if report.Error == "" {
			t.Error("fail conflict report has no error")
		}
	})

	t.Run("skip_does_not_create", func(t *testing.T) {
		var report agentImportReport
		testutil.Call(t, testHandler.ImportAgents,
			importExportRequest("POST", "/api/agents/import?on_conflict=skip", marshalExportFile(t, exp))).
			Want(http.StatusOK).JSON(&report)
		if len(report.Results) != 1 || report.Results[0].Status != "skipped" {
			t.Fatalf("results = %+v, want one skipped", report.Results)
		}
		if got := dbfx.Count(t, `SELECT count(*) FROM agent WHERE name = $1 AND workspace_id = $2`, name, testWorkspaceID); got != 1 {
			t.Errorf("agents named %q = %d, want 1", name, got)
		}
	})

	t.Run("rename_appends_suffix", func(t *testing.T) {
		var report agentImportReport
		req := importExportRequest("POST", "/api/agents/import?on_conflict=rename", marshalExportFile(t, exp))
		testutil.Call(t, testHandler.ImportAgents, req).Want(http.StatusOK).JSON(&report)
		if len(report.Results) != 1 {
			t.Fatalf("results = %d, want 1", len(report.Results))
		}
		if report.Results[0].Status != "renamed" || report.Results[0].Name != name+" (imported)" {
			t.Fatalf("result = %+v, want renamed to %q", report.Results[0], name+" (imported)")
		}
		dbfx.Cleanup(t, `DELETE FROM agent WHERE id = $1`, report.Results[0].ID)
	})
}

func TestAgentImport_RestoresArchivedAgents(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := "archived-restore" + t.Name()

	exp := agentExport{
		Version: agentExportVersion,
		Kind:    agentExportKind,
		Agents:  []agentExportEntry{{Name: name, RuntimeName: "Handler Test Runtime", Archived: true}},
	}

	var report agentImportReport
	req := importExportRequest("POST", "/api/agents/import?on_conflict=skip", marshalExportFile(t, exp))
	testutil.Call(t, testHandler.ImportAgents, req).Want(http.StatusOK).JSON(&report)
	if len(report.Results) != 1 || report.Results[0].Status != "created" {
		t.Fatalf("results = %+v, want created", report.Results)
	}
	defer dbfx.Cleanup(t, `DELETE FROM agent WHERE id = $1`, report.Results[0].ID)

	stored, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(report.Results[0].ID))
	if err != nil {
		t.Fatalf("load imported agent: %v", err)
	}
	if !stored.ArchivedAt.Valid {
		t.Error("archived agent was restored active, want archived")
	}
}
