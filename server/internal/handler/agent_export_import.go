package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agentconfig"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// agentExportKind is the "kind" discriminator written into an export file and
// required on import. It matches the CLI's 'multica agent export' contract
// (cmd/multica/cmd_agent_export_import.go), so a file produced by the web
// endpoint and one produced by the CLI are interchangeable on either side.
const agentExportKind = "multica-agent-export"

// agentExportVersion is the schema version of the export file. Bump it when
// the JSON layout changes incompatibly; import rejects anything newer.
const agentExportVersion = 1

// agentExport mirrors the CLI's agentExportFile. Field names and omission
// rules are part of the on-disk migration contract between two multica
// instances, so they must not drift from cmd/multica.
type agentExport struct {
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
	SourceID             string                     `json:"source_id,omitempty"`
	Name                 string                     `json:"name"`
	Archived             bool                       `json:"archived,omitempty"`
	RuntimeSourceID      string                     `json:"runtime_source_id,omitempty"`
	RuntimeName          string                     `json:"runtime_name,omitempty"`
	RuntimeProvider      string                     `json:"runtime_provider,omitempty"`
	Description          string                     `json:"description,omitempty"`
	Instructions         string                     `json:"instructions,omitempty"`
	ConversationStarters []AgentConversationStarter `json:"conversation_starters,omitempty"`
	Model                string                     `json:"model,omitempty"`
	ThinkingLevel        string                     `json:"thinking_level,omitempty"`
	ServiceTier          string                     `json:"service_tier,omitempty"`
	CustomArgs           []string                   `json:"custom_args,omitempty"`
	MaxConcurrentTasks   *int32                     `json:"max_concurrent_tasks,omitempty"`
	PermissionMode       string                     `json:"permission_mode,omitempty"`
	CustomEnv            map[string]string          `json:"custom_env,omitempty"`
	McpConfig            json.RawMessage            `json:"mcp_config,omitempty"`
	SkillNames           []string                   `json:"skill_names,omitempty"`
	Notes                []string                   `json:"notes,omitempty"`
}

// agentImportReport is the response shape of POST /api/agents/import: a
// per-agent verdict plus (on on_conflict=fail) an overall error.
type agentImportReport struct {
	Error   string              `json:"error,omitempty"`
	Results []agentImportResult `json:"results"`
}

type agentImportResult struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	ID       string   `json:"id,omitempty"`
	SourceID string   `json:"source_id,omitempty"`
	Runtime  string   `json:"runtime,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// ExportAgents serialises the workspace's user agents into a portable JSON
// file. Only a workspace owner or admin may export, and the file includes
// plaintext secrets (custom_env, mcp_config) on that exact basis.
//
// The wire format is shared with the CLI's `multica agent export`
// (cmd/multica/cmd_agent_export_import.go), so the exported JSON can be
// restored by either the web import endpoint on a target instance or the CLI.
func (h *Handler) ExportAgents(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	// Agent actors never see secrets (MUL-2600): an agent running under an
	// owner's daemon cannot use its host's credentials to dump the workspace's
	// plaintext custom_env / mcp_config.
	if actorType, _ := h.resolveActor(r, userID, workspaceID); actorType == "agent" {
		writeError(w, http.StatusForbidden, "agents may not export agent configurations")
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	ctx := r.Context()
	var err error
	var agents []db.Agent
	if r.URL.Query().Get("include_archived") == "true" {
		agents, err = h.Queries.ListAllAgents(ctx, wsUUID)
	} else {
		agents, err = h.Queries.ListAgents(ctx, wsUUID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list agents")
		return
	}

	runtimes, err := h.Queries.ListAgentRuntimes(ctx, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runtimes")
		return
	}
	runtimeByID := make(map[string]db.AgentRuntime, len(runtimes))
	for i := range runtimes {
		runtimeByID[uuidToString(runtimes[i].ID)] = runtimes[i]
	}

	skillRows, err := h.Queries.ListAgentSkillsByWorkspace(ctx, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load agent skills")
		return
	}
	bound := make(map[string][]AgentSkillSummary)
	for _, row := range skillRows {
		agentID := uuidToString(row.AgentID)
		bound[agentID] = append(bound[agentID], AgentSkillSummary{ID: uuidToString(row.ID), Name: row.Name})
	}

	export := agentExport{
		Version:    agentExportVersion,
		Kind:       agentExportKind,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Source:     agentExportSource{WorkspaceID: workspaceID},
	}
	for i := range runtimes {
		rt := &runtimes[i]
		export.Runtimes = append(export.Runtimes, agentExportRuntime{
			SourceID: uuidToString(rt.ID),
			Name:     agentRuntimeDisplayName(*rt),
			Provider: rt.Provider,
		})
	}

	skillDefs := make(map[string]agentExportSkill)
	for i := range agents {
		a := &agents[i]
		// Product-managed agents (system_key) cannot be meaningfully recreated
		// on another instance; skip them. The user-kind queries already exclude
		// them, but keep the guard for when the query contract changes.
		if a.SystemKey.Valid && a.SystemKey.String != "" {
			continue
		}
		entry := agentExportEntry{
			SourceID:       uuidToString(a.ID),
			Name:           a.Name,
			Archived:       a.ArchivedAt.Valid,
			Description:    a.Description,
			Instructions:   a.Instructions,
			PermissionMode: a.PermissionMode,
		}
		if rt, ok := runtimeByID[uuidToString(a.RuntimeID)]; ok {
			entry.RuntimeSourceID = uuidToString(rt.ID)
			entry.RuntimeName = agentRuntimeDisplayName(rt)
			entry.RuntimeProvider = rt.Provider
		}
		if a.Model.Valid {
			entry.Model = a.Model.String
		}
		if a.ThinkingLevel.Valid {
			entry.ThinkingLevel = a.ThinkingLevel.String
		}
		if a.ServiceTier.Valid {
			entry.ServiceTier = a.ServiceTier.String
		}
		// Historical rows may predate the max_concurrent_tasks invariant;
		// omit invalid values so import applies its default.
		if agentconfig.ValidateMaxConcurrentTasks(a.MaxConcurrentTasks) == nil {
			v := a.MaxConcurrentTasks
			entry.MaxConcurrentTasks = &v
		}
		if len(a.ConversationStarters) > 0 {
			var starters []AgentConversationStarter
			if json.Unmarshal(a.ConversationStarters, &starters) == nil && len(starters) > 0 {
				entry.ConversationStarters = starters
			}
		}
		if len(a.CustomArgs) > 0 {
			var args []string
			if json.Unmarshal(a.CustomArgs, &args) == nil && len(args) > 0 {
				entry.CustomArgs = args
			}
		}
		// Owner/admin can read every plaintext secret in the workspace, so an
		// export never carries a redaction note.
		entry.CustomEnv = unmarshalCustomEnv(*a)
		if len(entry.CustomEnv) == 0 {
			entry.CustomEnv = nil
		}
		if len(a.McpConfig) > 0 {
			entry.McpConfig = json.RawMessage(append([]byte(nil), a.McpConfig...))
		}

		for _, s := range bound[uuidToString(a.ID)] {
			if s.Name != "" {
				entry.SkillNames = append(entry.SkillNames, s.Name)
			}
			if s.ID == "" {
				continue
			}
			if _, seen := skillDefs[s.ID]; seen {
				continue
			}
			skill, err := h.Queries.GetSkill(ctx, parseUUID(s.ID))
			if err != nil {
				entry.Notes = append(entry.Notes, fmt.Sprintf("skill %q could not be read and will not be exported", s.Name))
				continue
			}
			files, err := h.Queries.ListSkillFiles(ctx, skill.ID)
			if err != nil {
				entry.Notes = append(entry.Notes, fmt.Sprintf("skill %q files could not be read and will not be exported", s.Name))
				continue
			}
			def := agentExportSkill{
				SourceID:    s.ID,
				Name:        skill.Name,
				Description: skill.Description,
				Content:     skill.Content,
				Config:      decodeSkillConfig(skill.Config),
			}
			for _, f := range files {
				def.Files = append(def.Files, agentExportSkillFile{Path: f.Path, Content: f.Content})
			}
			skillDefs[s.ID] = def
		}

		export.Agents = append(export.Agents, entry)
	}

	if len(export.Agents) == 0 {
		writeError(w, http.StatusBadRequest, "no agents to export")
		return
	}

	for _, def := range skillDefs {
		export.Skills = append(export.Skills, def)
	}
	sort.Slice(export.Skills, func(i, j int) bool { return export.Skills[i].Name < export.Skills[j].Name })

	writeJSON(w, http.StatusOK, export)
}

// ImportAgents restores a prior agent-export file into the current workspace.
// Only a workspace owner or admin may import. Skills are matched by name: a
// skill already present on the target is reused as-is, a missing one is
// recreated from the exported definition. Runtimes are matched by display name
// (custom_name or name, case-insensitive), or via the optional
// default_runtime_id query parameter. on_conflict controls what happens when
// an agent with the same name already exists (default skip).
func (h *Handler) ImportAgents(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if actorType, _ := h.resolveActor(r, userID, workspaceID); actorType == "agent" {
		writeError(w, http.StatusForbidden, "agents may not import agent configurations")
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin")
	if !ok {
		return
	}

	onConflict := r.URL.Query().Get("on_conflict")
	if onConflict == "" {
		onConflict = "skip"
	}
	switch onConflict {
	case "fail", "skip", "rename":
	default:
		writeError(w, http.StatusBadRequest, "on_conflict must be one of: fail, skip, rename")
		return
	}
	var defaultRuntimeUUID pgtype.UUID
	if dr := r.URL.Query().Get("default_runtime_id"); dr != "" {
		parsed, ok := parseUUIDOrBadRequest(w, dr, "default_runtime_id")
		if !ok {
			return
		}
		defaultRuntimeUUID = parsed
	}

	r.Body = http.MaxBytesReader(w, r.Body, 96<<20)
	var export agentExport
	if err := json.NewDecoder(r.Body).Decode(&export); err != nil {
		writeError(w, http.StatusBadRequest, "invalid export file: "+err.Error())
		return
	}
	if !strings.EqualFold(strings.TrimSpace(export.Kind), agentExportKind) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("not a %s file (kind=%q)", agentExportKind, export.Kind))
		return
	}
	if export.Version != agentExportVersion {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported export version %d (this server reads version %d)", export.Version, agentExportVersion))
		return
	}
	if len(export.Agents) == 0 {
		writeError(w, http.StatusBadRequest, "export file contains no agents")
		return
	}

	ctx := r.Context()

	// Existing names index: creation is gated by the UNIQUE(workspace_id, name)
	// constraint and archived agents still occupy their name, so the conflict
	// set is every agent the workspace has ever contained.
	existing, err := h.Queries.ListAllAgents(ctx, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list existing agents")
		return
	}
	existingNames := make(map[string]struct{}, len(existing))
	for i := range existing {
		existingNames[existing[i].Name] = struct{}{}
	}

	runtimes, err := h.Queries.ListAgentRuntimes(ctx, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list target runtimes")
		return
	}
	runtimeByName := make(map[string]db.AgentRuntime, len(runtimes))
	runtimeByID := make(map[string]db.AgentRuntime, len(runtimes))
	for i := range runtimes {
		rt := &runtimes[i]
		if d := strings.ToLower(agentRuntimeDisplayName(*rt)); d != "" {
			runtimeByName[d] = *rt
		}
		runtimeByID[uuidToString(rt.ID)] = *rt
	}

	// Reuse-or-recreate skills. A missing skill is created from the exported
	// definition before any agent, so binding never points at a dangling id.
	skillIDByName := make(map[string]string)
	for i := range export.Skills {
		sk := &export.Skills[i]
		if sk.Name == "" {
			continue
		}
		identity, exists, err := h.existingSkillIdentityByName(ctx, wsUUID, sk.Name)
		if err != nil {
			slog.Warn("import: skill lookup failed", append(logger.RequestAttrs(r), "error", err, "skill", sk.Name)...)
			continue
		}
		if exists {
			skillIDByName[sk.Name] = identity.ID
			continue
		}
		files := make([]CreateSkillFileRequest, 0, len(sk.Files))
		for _, f := range sk.Files {
			files = append(files, CreateSkillFileRequest{Path: f.Path, Content: f.Content})
		}
		created, cerr := h.createSkillWithFiles(ctx, skillCreateInput{
			WorkspaceID: wsUUID,
			CreatorID:   parseUUID(userID),
			Name:        sk.Name,
			Description: sk.Description,
			Content:     sk.Content,
			Config:      sk.Config,
			Files:       files,
		})
		if cerr != nil {
			slog.Warn("import: skill create failed", append(logger.RequestAttrs(r), "error", cerr, "skill", sk.Name)...)
			continue
		}
		skillIDByName[sk.Name] = created.SkillResponse.ID
	}

	results := make([]agentImportResult, 0, len(export.Agents))
	for i := range export.Agents {
		e := &export.Agents[i]
		res := agentImportResult{Name: e.Name, SourceID: e.SourceID}

		rt, problem := resolveImportTargetRuntime(e, runtimeByName, runtimeByID, defaultRuntimeUUID)
		if problem != "" {
			res.Status = "failed"
			res.Error = problem
			results = append(results, res)
			continue
		}
		res.Runtime = uuidToString(rt.ID)
		if !canUseRuntimeForAgent(member, rt) {
			res.Status = "failed"
			res.Error = "this runtime is private; only its owner can create agents on it"
			results = append(results, res)
			continue
		}

		skillIDs, skillNotes := resolveImportSkillIDs(e.SkillNames, skillIDByName)
		res.Notes = append(res.Notes, skillNotes...)
		if e.PermissionMode == "public_to" {
			res.Notes = append(res.Notes, "invocation allow-list not migrated; re-grant members after import")
		}

		starters, err := normaliseAgentConversationStarters(e.ConversationStarters)
		if err != nil {
			res.Status = "failed"
			res.Error = err.Error()
			results = append(results, res)
			continue
		}

		name := e.Name
		if _, exists := existingNames[name]; exists {
			switch onConflict {
			case "fail":
				res.Status = "failed"
				res.Error = fmt.Sprintf("an agent named %q already exists on the target", name)
				results = append(results, res)
				writeJSON(w, http.StatusConflict, agentImportReport{Error: res.Error, Results: results})
				return
			case "skip":
				res.Status = "skipped"
				res.Error = "already exists on the target"
				results = append(results, res)
				continue
			case "rename":
				name = e.Name + " (imported)"
				if _, exists := existingNames[name]; exists {
					res.Status = "failed"
					res.Error = fmt.Sprintf("an agent named %q already exists on the target", name)
					results = append(results, res)
					continue
				}
			}
		}

		maxCT := agentconfig.DefaultMaxConcurrentTasks
		if e.MaxConcurrentTasks != nil {
			if err := agentconfig.ValidateMaxConcurrentTasks(*e.MaxConcurrentTasks); err == nil {
				maxCT = *e.MaxConcurrentTasks
			}
		}

		// A renamed agent reports its new name so the caller can reconcile.
		if name != e.Name {
			res.Name = name
		}

		created, err := h.insertAgentFromExport(ctx, wsUUID, userID, rt, name, e, starters, maxCT, skillIDs)
		if err != nil {
			res.Status = "failed"
			if isUniqueViolation(err) {
				res.Error = fmt.Sprintf("an agent named %q already exists on the target", name)
			} else {
				res.Error = err.Error()
			}
			results = append(results, res)
			continue
		}
		createdID := uuidToString(created.ID)
		res.ID = createdID
		if name != e.Name {
			res.Status = "renamed"
		} else {
			res.Status = "created"
		}

		resp := h.agentToResponse(created)
		if err := h.attachAgentSkills(ctx, &resp, created.ID); err != nil {
			slog.Warn("import: load skills for response failed", append(logger.RequestAttrs(r), "error", err, "agent_id", createdID)...)
		}
		h.publish(protocol.EventAgentCreated, workspaceID, "member", userID, map[string]any{"agent": broadcastAgentResponse(resp)})

		if e.Archived {
			if _, err := h.Queries.ArchiveAgent(ctx, db.ArchiveAgentParams{ID: created.ID, ArchivedBy: parseUUID(userID)}); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("agent created but could not be archived: %v", err))
			} else if archived, err := h.Queries.GetAgent(ctx, created.ID); err == nil {
				aresp := h.agentToResponse(archived)
				h.attachAgentSkills(ctx, &aresp, archived.ID)
				h.publish(protocol.EventAgentArchived, workspaceID, "member", userID, map[string]any{"agent": broadcastAgentResponse(aresp)})
			}
		}

		results = append(results, res)
	}

	writeJSON(w, http.StatusOK, agentImportReport{Results: results})
}

// resolveImportTargetRuntime maps an exported agent to a target runtime row.
// It matches the exported display name case-insensitively, then falls back to
// defaultRuntimeID. Returns a non-empty problem description when nothing
// resolves.
func resolveImportTargetRuntime(e *agentExportEntry, byName map[string]db.AgentRuntime, byID map[string]db.AgentRuntime, defaultID pgtype.UUID) (db.AgentRuntime, string) {
	if e.RuntimeName != "" {
		if rt, ok := byName[strings.ToLower(e.RuntimeName)]; ok {
			return rt, ""
		}
	}
	if defaultID.Valid {
		if rt, ok := byID[uuidToString(defaultID)]; ok {
			return rt, ""
		}
	}
	if e.RuntimeName != "" {
		return db.AgentRuntime{}, fmt.Sprintf("could not match runtime %q on the target; pass default_runtime_id to resolve it", e.RuntimeName)
	}
	return db.AgentRuntime{}, "the exported agent has no runtime and no default_runtime_id was provided"
}

// resolveImportSkillIDs maps an exported agent's skill names to target skill
// ids. A name with no target equivalent is reported in notes: the skill was
// not on the target and could not be recreated.
func resolveImportSkillIDs(names []string, skillIDByName map[string]string) ([]pgtype.UUID, []string) {
	var ids []pgtype.UUID
	var missing []string
	for _, n := range names {
		if n == "" {
			continue
		}
		id := skillIDByName[n]
		if id == "" {
			missing = append(missing, n)
			continue
		}
		ids = append(ids, parseUUID(id))
	}
	var notes []string
	if len(missing) > 0 {
		notes = append(notes, "skills not bound (not on the target): "+strings.Join(missing, ", "))
	}
	return ids, notes
}

// insertAgentFromExport persists one imported agent and its skill bindings in
// a single transaction. custom_env and mcp_config are written verbatim from
// the export file (the importer is workspace owner/admin, so clear-text secret
// migration is authorized). Invocation permission carries its mode only; the
// allow-list is deliberately left empty and derived visibility stays private.
func (h *Handler) insertAgentFromExport(ctx context.Context, wsUUID pgtype.UUID, ownerID string, runtime db.AgentRuntime, name string, e *agentExportEntry, starters []AgentConversationStarter, maxCT int32, skillIDs []pgtype.UUID) (db.Agent, error) {
	ce := []byte("{}")
	if e.CustomEnv != nil {
		if b, err := json.Marshal(e.CustomEnv); err == nil {
			ce = b
		}
	}
	ca := []byte("[]")
	if e.CustomArgs != nil {
		if b, err := json.Marshal(e.CustomArgs); err == nil {
			ca = b
		}
	}
	sp, _ := json.Marshal(starters)

	var mc []byte
	if len(e.McpConfig) > 0 {
		mc = append([]byte(nil), e.McpConfig...)
	}

	permMode := "private"
	if e.PermissionMode == "public_to" {
		permMode = "public_to"
	}

	var model, thinking, tier pgtype.Text
	if e.Model != "" {
		model = pgtype.Text{String: e.Model, Valid: true}
	}
	if e.ThinkingLevel != "" {
		thinking = pgtype.Text{String: e.ThinkingLevel, Valid: true}
	}
	if e.ServiceTier != "" {
		tier = pgtype.Text{String: e.ServiceTier, Valid: true}
	}

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.Agent{}, err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)

	created, err := qtx.CreateAgent(ctx, db.CreateAgentParams{
		WorkspaceID:              wsUUID,
		Name:                     name,
		Description:              e.Description,
		AvatarUrl:                pgtype.Text{},
		RuntimeMode:              runtime.RuntimeMode,
		RuntimeConfig:            []byte("{}"),
		RuntimeID:                runtime.ID,
		Visibility:               "private",
		MaxConcurrentTasks:       maxCT,
		OwnerID:                  parseUUID(ownerID),
		Instructions:             e.Instructions,
		CustomEnv:                ce,
		CustomArgs:               ca,
		McpConfig:                mc,
		Model:                    model,
		ThinkingLevel:            thinking,
		ServiceTier:              tier,
		ConversationStarters:     sp,
		ComposioToolkitAllowlist: nil,
		PermissionMode:           permMode,
	})
	if err != nil {
		return db.Agent{}, err
	}

	for _, skillID := range skillIDs {
		if err := qtx.AddAgentSkill(ctx, db.AddAgentSkillParams{AgentID: created.ID, SkillID: skillID}); err != nil {
			return db.Agent{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return db.Agent{}, err
	}
	return created, nil
}

// agentRuntimeDisplayName returns the human-facing name of a runtime used to
// match it on another instance: custom_name when set, otherwise the daemon's
// name. Mirrors the CLI's runtimeDisplayName.
func agentRuntimeDisplayName(rt db.AgentRuntime) string {
	if rt.CustomName.Valid && rt.CustomName.String != "" {
		return rt.CustomName.String
	}
	return rt.Name
}
