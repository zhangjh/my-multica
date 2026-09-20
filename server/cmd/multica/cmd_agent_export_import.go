package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// agentExportKind is the "kind" discriminator written into an export file and
// required on import. It is part of the on-disk migration contract between two
// multica instances, so it must not change once released.
const agentExportKind = "multica-agent-export"

// agentExportVersion is the schema version of the export file. Bump it when
// the JSON layout changes incompatibly; import rejects anything newer.
const agentExportVersion = 1

// plannedSkillMarker marks a skill that import would create on a real run
// during --dry-run. It can never collide with a real id: skill ids are UUIDs
// while the marker is a single NUL byte.
const plannedSkillMarker = "\x00planned"

// ---------------------------------------------------------------------------
// Export / import file schema
// ---------------------------------------------------------------------------

type agentExportFile struct {
	Version    int                  `json:"version"`
	Kind       string               `json:"kind"`
	ExportedAt string               `json:"exported_at"`
	Source     agentExportSource    `json:"source"`
	Runtimes   []agentExportRuntime `json:"runtimes,omitempty"`
	Skills     []agentExportSkill   `json:"skills,omitempty"`
	Agents     []agentExportEntry   `json:"agents"`
}

type agentExportSource struct {
	ServerURL   string `json:"server_url,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

type agentExportRuntime struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type agentExportSkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type agentExportSkill struct {
	SourceID    string                 `json:"source_id"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Content     string                 `json:"content"`
	Config      any                    `json:"config,omitempty"`
	Files       []agentExportSkillFile `json:"files,omitempty"`
}

type agentExportEntry struct {
	SourceID             string            `json:"source_id,omitempty"`
	Name                 string            `json:"name"`
	Archived             bool              `json:"archived,omitempty"`
	RuntimeSourceID      string            `json:"runtime_source_id,omitempty"`
	RuntimeName          string            `json:"runtime_name,omitempty"`
	RuntimeProvider      string            `json:"runtime_provider,omitempty"`
	Description          string            `json:"description,omitempty"`
	Instructions         string            `json:"instructions,omitempty"`
	ConversationStarters []any             `json:"conversation_starters,omitempty"`
	Model                string            `json:"model,omitempty"`
	ThinkingLevel        string            `json:"thinking_level,omitempty"`
	ServiceTier          string            `json:"service_tier,omitempty"`
	CustomArgs           []any             `json:"custom_args,omitempty"`
	MaxConcurrentTasks   *int32            `json:"max_concurrent_tasks,omitempty"`
	PermissionMode       string            `json:"permission_mode,omitempty"`
	CustomEnv            map[string]string `json:"custom_env,omitempty"`
	McpConfig            json.RawMessage   `json:"mcp_config,omitempty"`
	SkillNames           []string          `json:"skill_names,omitempty"`
	Notes                []string          `json:"notes,omitempty"`
}

// ---------------------------------------------------------------------------
// Commands and flags
// ---------------------------------------------------------------------------

var agentExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export agent configurations to a JSON file for migration between instances",
	Long: `Export the current workspace's agents into a JSON file that 'multica agent
import' can restore on another instance.

The export captures every portable agent field — instructions, conversation
starters, model, thinking_level, service_tier, custom_args, max_concurrent_tasks
and permission mode — plus custom_env and, when your account can read them,
mcp_config. Skills are exported as full definitions (SKILL.md plus supporting
files) so 'agent import' can recreate and re-bind them by name on the target.

Not carried across the file: runtime_config (machine-local, gateway tokens are
masked anyway), and avatar URLs (a URL from one instance does not resolve on
another).

The exported file contains secrets in the clear (custom_env / mcp_config are
written verbatim when readable) and is written with mode 0600. Treat it like a
credential: keep it out of shared drives and source control.`,
	Args: cobra.NoArgs,
	RunE: runAgentExport,
}

var agentImportCmd = &cobra.Command{
	Use:   "import --file <export.json>",
	Short: "Import agents from a 'multica agent export' JSON file",
	Long: `Restore agents previously written by 'multica agent export' into the current
workspace of this instance.

Runtimes are matched by display name (custom_name or name, case-insensitive).
Map a source runtime explicitly with --runtime "<source-name>=<target-id>"
(repeatable), or fall back to --default-runtime when a name does not match.

Skills are matched by name. A skill name already present on the target is
reused as-is; a missing skill is recreated from the exported definition and the
exported agents are bound to it. Use --no-skills to skip skill handling.

Invocation permission is migrated as its mode only (private / public_to); the
per-member or workspace allow-list is deliberately dropped, so a public_to
agent needs its members re-granted after import. custom_env and mcp_config are
restored from the export file.

Run with --dry-run to report what would happen without changing anything.`,
	Args: cobra.NoArgs,
	RunE: runAgentImport,
}

func init() {
	agentCmd.AddCommand(agentExportCmd)
	agentCmd.AddCommand(agentImportCmd)
	registerAgentExportFlags(agentExportCmd)
	registerAgentImportFlags(agentImportCmd)
}

// registerAgentExportFlags registers every flag runAgentExport reads. It is
// shared between init() and the tests so both stay in lockstep.
func registerAgentExportFlags(cmd *cobra.Command) {
	cmd.Flags().String("output", "agents-export.json", "Write the export to this path, or '-' for stdout")
	cmd.Flags().StringSlice("agent", nil, "Only export these source agent id(s). Repeatable.")
	cmd.Flags().Bool("include-archived", false, "Also export archived agents")
}

// registerAgentImportFlags registers every flag runAgentImport reads. It is
// shared between init() and the tests so both stay in lockstep.
func registerAgentImportFlags(cmd *cobra.Command) {
	cmd.Flags().String("file", "", "Path to the 'multica agent export' file to import, or '-' for stdin. (required)")
	cmd.Flags().String("default-runtime", "", "Target runtime ID to use for agents whose source runtime cannot be matched by name")
	cmd.Flags().StringSlice("runtime", nil, "Map a source runtime name (or id) to a target runtime id: <source>=<target-id>. Repeatable.")
	cmd.Flags().String("on-conflict", "fail", "How to handle an agent that already exists on the target: fail, skip, or rename")
	cmd.Flags().Bool("no-skills", false, "Skip skill recreation and binding; import agents without skill_ids")
	cmd.Flags().Bool("dry-run", false, "Resolve every agent and skill but do not create or archive anything")
	cmd.Flags().String("output", "table", "Report format: table or json")
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

func runAgentExport(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}

	includeArchived, _ := cmd.Flags().GetBool("include-archived")
	onlyAgents, _ := cmd.Flags().GetStringSlice("agent")

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var agents []map[string]any
	if err := client.GetJSON(ctx, "/api/agents", &agents); err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	var runtimes []map[string]any
	if err := client.GetJSON(ctx, "/api/runtimes", &runtimes); err != nil {
		return fmt.Errorf("list runtimes: %w", err)
	}
	runtimeByID := make(map[string]map[string]any, len(runtimes))
	for _, rt := range runtimes {
		runtimeByID[strVal(rt, "id")] = rt
	}

	export := agentExportFile{
		Version:    agentExportVersion,
		Kind:       agentExportKind,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Source:     agentExportSource{ServerURL: client.BaseURL, WorkspaceID: client.WorkspaceID},
	}
	for _, rt := range runtimes {
		export.Runtimes = append(export.Runtimes, agentExportRuntime{
			SourceID: strVal(rt, "id"),
			Name:     runtimeDisplayName(rt),
			Provider: strVal(rt, "provider"),
		})
	}

	filter := make(map[string]bool, len(onlyAgents))
	for _, id := range onlyAgents {
		filter[id] = true
	}

	skillSeen := make(map[string]bool)
	warnf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...) }

	for _, a := range agents {
		id := strVal(a, "id")
		if len(filter) > 0 && !filter[id] {
			continue
		}

		// Product-managed agents (system_key) cannot be meaningfully
		// recreated on another instance; skip them loudly.
		if systemKey := strVal(a, "system_key"); systemKey != "" {
			warnf("skipping system agent %q (%s): managed by the product, not portable", strVal(a, "name"), id)
			continue
		}
		if !includeArchived && strVal(a, "archived_at") != "" {
			continue
		}

		entry := agentExportEntry{
			SourceID:       id,
			Name:           strVal(a, "name"),
			Archived:       strVal(a, "archived_at") != "",
			Description:    strVal(a, "description"),
			Instructions:   strVal(a, "instructions"),
			Model:          strVal(a, "model"),
			ThinkingLevel:  strVal(a, "thinking_level"),
			ServiceTier:    strVal(a, "service_tier"),
			PermissionMode: strVal(a, "permission_mode"),
		}
		if cs, ok := a["conversation_starters"].([]any); ok && len(cs) > 0 {
			entry.ConversationStarters = cs
		}
		if ca, ok := a["custom_args"].([]any); ok && len(ca) > 0 {
			entry.CustomArgs = ca
		}
		// Historical rows may predate the max_concurrent_tasks invariant;
		// omit invalid values so create applies its default on import.
		if v, ok := copiedAgentMaxConcurrentTasks(a["max_concurrent_tasks"]); ok {
			entry.MaxConcurrentTasks = &v
		}

		rt := runtimeByID[strVal(a, "runtime_id")]
		entry.RuntimeSourceID = strVal(rt, "id")
		entry.RuntimeName = runtimeDisplayName(rt)
		entry.RuntimeProvider = strVal(rt, "provider")

		// Skills: the agent carries only summaries; fetch each full definition
		// (SKILL.md + files) so import can recreate it on the target.
		if skills, ok := a["skills"].([]any); ok {
			for _, s := range skills {
				m, _ := s.(map[string]any)
				if m == nil {
					continue
				}
				skillID := strVal(m, "id")
				skillName := strVal(m, "name")
				if skillName != "" {
					entry.SkillNames = append(entry.SkillNames, skillName)
				}
				if skillID == "" || skillSeen[skillID] {
					continue
				}
				skillSeen[skillID] = true

				var full map[string]any
				if err := client.GetJSON(ctx, "/api/skills/"+url.PathEscape(skillID)+"?include=content", &full); err != nil {
					warnf("could not read skill %q (%s): %v; agents referencing it will skip binding on import", skillName, skillID, err)
					continue
				}
				sk := agentExportSkill{
					SourceID:    skillID,
					Name:        strVal(full, "name"),
					Description: strVal(full, "description"),
					Content:     strVal(full, "content"),
					Config:      full["config"],
				}
				if files, ok := full["files"].([]any); ok {
					for _, f := range files {
						fm, _ := f.(map[string]any)
						if fm == nil {
							continue
						}
						sk.Files = append(sk.Files, agentExportSkillFile{
							Path:    strVal(fm, "path"),
							Content: strVal(fm, "content"),
						})
					}
				}
				export.Skills = append(export.Skills, sk)
			}
		}

		// custom_env lives behind its own audited endpoint and is only
		// readable by the agent owner or a workspace owner/admin. A refusal
		// is a per-agent warning, not a reason to abort the whole export.
		var envResp map[string]any
		if err := client.GetJSON(ctx, "/api/agents/"+url.PathEscape(id)+"/env", &envResp); err != nil {
			entry.Notes = append(entry.Notes, "could not read custom_env (must be the agent owner or a workspace owner/admin); it will not be exported")
		} else if env, ok := envResp["custom_env"].(map[string]any); ok {
			entry.CustomEnv = flattenEnv(env)
		}

		// mcp_config is plaintext only for the agent owner or a workspace
		// owner/admin; otherwise the server redacts it.
		if redacted, ok := a["mcp_config_redacted"].(bool); ok && redacted {
			entry.Notes = append(entry.Notes, "mcp_config is redacted for this account; it will not be exported")
		} else if mc, ok := a["mcp_config"]; ok && mc != nil {
			raw, err := json.Marshal(mc)
			if err != nil {
				return fmt.Errorf("encode mcp_config for %q: %w", entry.Name, err)
			}
			entry.McpConfig = raw
		}

		export.Agents = append(export.Agents, entry)
	}

	if len(export.Agents) == 0 {
		return fmt.Errorf("no agents to export (use --include-archived to include archived agents)")
	}

	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	out, _ := cmd.Flags().GetString("output")
	if out == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Exported %d agent(s) and %d skill(s) to stdout\n", len(export.Agents), len(export.Skills))
		return nil
	}

	// The file carries custom_env / mcp_config secrets in the clear.
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Exported %d agent(s) and %d skill(s) to %s\n", len(export.Agents), len(export.Skills), out)
	return nil
}

// runtimeDisplayName returns the human-facing name of a runtime used to match
// it on another instance: custom_name when set, otherwise the daemon's name.
func runtimeDisplayName(rt map[string]any) string {
	if rt == nil {
		return ""
	}
	if cn := strVal(rt, "custom_name"); cn != "" {
		return cn
	}
	return strVal(rt, "name")
}

// flattenEnv converts an /env response's json-typed custom_env map into
// plain strings for storage in the export file.
func flattenEnv(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case nil:
			out[k] = ""
		default:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

type importAgentResult struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	ID       string   `json:"id,omitempty"`
	SourceID string   `json:"source_id,omitempty"`
	Runtime  string   `json:"runtime,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Error    string   `json:"error,omitempty"`
}

func runAgentImport(cmd *cobra.Command, _ []string) error {
	fileFlag, _ := cmd.Flags().GetString("file")
	if fileFlag == "" {
		return fmt.Errorf("--file is required (path to a 'multica agent export' file, or '-' for stdin)")
	}

	var raw []byte
	var err error
	if fileFlag == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(fileFlag)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", fileFlag, err)
	}

	var export agentExportFile
	if err := json.Unmarshal(raw, &export); err != nil {
		return fmt.Errorf("parse export file: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(export.Kind)) != agentExportKind {
		return fmt.Errorf("%s is not a %s file (kind=%q)", fileFlag, agentExportKind, export.Kind)
	}
	if export.Version != agentExportVersion {
		return fmt.Errorf("unsupported export version %d (this CLI reads version %d)", export.Version, agentExportVersion)
	}
	if len(export.Agents) == 0 {
		return fmt.Errorf("export file contains no agents")
	}

	onConflict, _ := cmd.Flags().GetString("on-conflict")
	switch onConflict {
	case "fail", "skip", "rename":
	default:
		return fmt.Errorf("--on-conflict must be one of: fail, skip, rename")
	}
	noSkills, _ := cmd.Flags().GetBool("no-skills")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	defaultRuntime, _ := cmd.Flags().GetString("default-runtime")

	runtimeOverrides := make(map[string]string)
	for _, pair := range mustStringSlice(cmd, "runtime") {
		name, id, _ := strings.Cut(pair, "=")
		if name == "" || id == "" {
			return fmt.Errorf("--runtime must be <source-name>=<target-id>, got %q", pair)
		}
		runtimeOverrides[name] = id
	}

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}

	// Target runtime index: lowercased display name -> id.
	targetRuntimes := make(map[string]string)
	var rtList []map[string]any
	if err := client.GetJSON(ctx, "/api/runtimes", &rtList); err != nil {
		if defaultRuntime == "" {
			return fmt.Errorf("list target runtimes: %w (pass --default-runtime to skip runtime matching)", err)
		}
		fmt.Fprintf(os.Stderr, "warning: could not list target runtimes (%v); using --default-runtime for every agent\n", err)
	} else {
		for _, rt := range rtList {
			if d := strings.ToLower(runtimeDisplayName(rt)); d != "" {
				targetRuntimes[d] = strVal(rt, "id")
			}
		}
	}

	// Skills: reuse by name, recreate when missing. During --dry-run the
	// "planned create" placeholder keeps binding logic intact without writing.
	skillIDByName := make(map[string]string)
	if !noSkills {
		var existing []map[string]any
		if err := client.GetJSON(ctx, "/api/skills", &existing); err != nil {
			return fmt.Errorf("list target skills: %w", err)
		}
		for _, s := range existing {
			if n := strVal(s, "name"); n != "" {
				skillIDByName[n] = strVal(s, "id")
			}
		}
		for i := range export.Skills {
			sk := &export.Skills[i]
			if sk.Name == "" {
				continue
			}
			if _, ok := skillIDByName[sk.Name]; ok {
				continue // reuse the existing target skill
			}
			if dryRun {
				skillIDByName[sk.Name] = plannedSkillMarker
				continue
			}
			body := map[string]any{"name": sk.Name}
			if sk.Description != "" {
				body["description"] = sk.Description
			}
			if sk.Content != "" {
				body["content"] = sk.Content
			}
			if sk.Config != nil {
				body["config"] = sk.Config
			}
			if len(sk.Files) > 0 {
				files := make([]any, 0, len(sk.Files))
				for _, f := range sk.Files {
					files = append(files, map[string]any{"path": f.Path, "content": f.Content})
				}
				body["files"] = files
			}
			var created map[string]any
			if err := client.PostJSON(ctx, "/api/skills", body, &created); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not create skill %q: %v; agents referencing it will be imported without it\n", sk.Name, err)
				continue
			}
			skillIDByName[sk.Name] = strVal(created, "id")
		}
	}

	results := make([]importAgentResult, 0, len(export.Agents))
	failures := 0
	for i := range export.Agents {
		e := &export.Agents[i]
		res := importAgentResult{Name: e.Name, SourceID: e.SourceID}

		runtimeID, runtimeProblem := resolveTargetRuntime(e, runtimeOverrides, targetRuntimes, defaultRuntime)
		if runtimeProblem != "" {
			res.Status = "failed"
			res.Error = runtimeProblem
			results = append(results, res)
			failures++
			continue
		}
		res.Runtime = runtimeID

		skillIDs, skillNotes := resolveSkillIDs(e.SkillNames, skillIDByName, noSkills)
		res.Notes = append(res.Notes, skillNotes...)
		if e.PermissionMode == "public_to" {
			res.Notes = append(res.Notes, "invocation allow-list not migrated; re-grant members after import")
		}

		body := map[string]any{
			"name":       e.Name,
			"runtime_id": runtimeID,
		}
		putNonEmpty(body, "description", e.Description)
		putNonEmpty(body, "instructions", e.Instructions)
		putNonEmpty(body, "model", e.Model)
		putNonEmpty(body, "thinking_level", e.ThinkingLevel)
		putNonEmpty(body, "service_tier", e.ServiceTier)
		if len(e.ConversationStarters) > 0 {
			body["conversation_starters"] = e.ConversationStarters
		}
		if len(e.CustomArgs) > 0 {
			body["custom_args"] = e.CustomArgs
		}
		if e.MaxConcurrentTasks != nil {
			body["max_concurrent_tasks"] = *e.MaxConcurrentTasks
		}
		if e.PermissionMode != "" {
			// Mode only: the invocation allow-list is deliberately not
			// migrated (see the command doc).
			body["permission_mode"] = e.PermissionMode
		}
		if e.CustomEnv != nil {
			body["custom_env"] = e.CustomEnv
		}
		if len(e.McpConfig) > 0 {
			body["mcp_config"] = e.McpConfig
		}
		if len(skillIDs) > 0 {
			body["skill_ids"] = skillIDs
		}

		if dryRun {
			res.Status = "dry-run"
			res.Notes = append(res.Notes, "no changes made (--dry-run)")
			results = append(results, res)
			continue
		}

		var created map[string]any
		if err := client.PostJSON(ctx, "/api/agents", body, &created); err != nil {
			var httpErr *cli.HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict {
				res.Status = "failed"
				res.Error = err.Error()
				results = append(results, res)
				failures++
				continue
			}
			switch onConflict {
			case "fail":
				_ = printImportReport(cmd, results...)
				return fmt.Errorf("agent %q already exists on the target (--on-conflict fail); re-run with --on-conflict skip or rename to continue", e.Name)
			case "skip":
				res.Status = "skipped"
				res.Error = "already exists on the target"
				results = append(results, res)
				continue
			case "rename":
				body["name"] = e.Name + " (imported)"
				if err := client.PostJSON(ctx, "/api/agents", body, &created); err != nil {
					res.Status = "failed"
					res.Error = fmt.Sprintf("conflict rename retry: %v", err)
					results = append(results, res)
					failures++
					continue
				}
				res.Status = "renamed"
				res.Name = strVal(created, "name")
			}
		} else {
			res.Status = "created"
		}
		if res.Status == "created" || res.Status == "renamed" {
			res.ID = strVal(created, "id")
		}

		if e.Archived && res.ID != "" {
			var arch map[string]any
			if err := client.PostJSON(ctx, "/api/agents/"+res.ID+"/archive", nil, &arch); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("agent created but could not be archived: %v", err))
				failures++
			}
		}
		results = append(results, res)
	}

	if err := printImportReport(cmd, results...); err != nil {
		return err
	}
	if failures > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "%d agent(s) failed to import", failures)
		for _, r := range results {
			if r.Status == "failed" && r.Error != "" {
				fmt.Fprintf(&sb, "; %s: %s", r.Name, r.Error)
			}
		}
		return fmt.Errorf("%s", sb.String())
	}
	return nil
}

// resolveTargetRuntime maps an exported agent to a target runtime id. It
// prefers an explicit --runtime override (matched against the source display
// name, then its id), falls back to a case-insensitive name match on the
// target, then to --default-runtime. Returns "" with a problem description
// when nothing resolves.
func resolveTargetRuntime(e *agentExportEntry, overrides map[string]string, targetIndex map[string]string, defaultRuntime string) (string, string) {
	for _, key := range []string{e.RuntimeName, e.RuntimeSourceID} {
		if key == "" {
			continue
		}
		if id, ok := overrides[key]; ok {
			return id, ""
		}
	}
	if e.RuntimeName != "" {
		if id, ok := targetIndex[strings.ToLower(e.RuntimeName)]; ok {
			return id, ""
		}
	}
	if defaultRuntime != "" {
		return defaultRuntime, ""
	}
	label := e.RuntimeName
	if label == "" {
		label = "the source runtime (id " + e.RuntimeSourceID + ")"
	}
	return "", fmt.Sprintf("could not match %s to a target runtime; pass --runtime %s=<target-id> or --default-runtime <id>", label, e.RuntimeName)
}

// resolveSkillIDs maps an exported agent's skill names to target skill ids. A
// name with no target equivalent is reported in notes: with real runs the
// skill was not found (and was not createable), with --dry-run it would have
// been created.
func resolveSkillIDs(names []string, skillIDByName map[string]string, noSkills bool) ([]string, []string) {
	if noSkills {
		return nil, nil
	}
	var ids []string
	var planned, missing []string
	for _, n := range names {
		if n == "" {
			continue
		}
		switch id := skillIDByName[n]; {
		case id == "":
			missing = append(missing, n)
		case id == plannedSkillMarker:
			planned = append(planned, n)
		default:
			ids = append(ids, id)
		}
	}
	var notes []string
	if len(planned) > 0 {
		notes = append(notes, "skills that a real run would create: "+strings.Join(planned, ", "))
	}
	if len(missing) > 0 {
		notes = append(notes, "skills not bound (not on the target): "+strings.Join(missing, ", "))
	}
	return ids, notes
}

func printImportReport(cmd *cobra.Command, results ...importAgentResult) error {
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, results)
	}
	headers := []string{"NAME", "STATUS", "ID", "NOTES"}
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		notes := strings.Join(r.Notes, "; ")
		if r.Error != "" {
			if notes != "" {
				notes += "; "
			}
			notes += r.Error
		}
		rows = append(rows, []string{r.Name, r.Status, r.ID, notes})
	}
	cli.PrintTable(os.Stdout, headers, rows)
	return nil
}

// mustStringSlice reads a StringSlice flag, never erroring; the flag
// registration guarantees the reader succeeds.
func mustStringSlice(cmd *cobra.Command, name string) []string {
	v, _ := cmd.Flags().GetStringSlice(name)
	return v
}

// putNonEmpty adds key to m only when val is non-empty.
func putNonEmpty(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}
