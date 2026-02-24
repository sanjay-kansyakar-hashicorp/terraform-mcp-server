// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hashicorp/terraform-mcp-server/pkg/client"
	log "github.com/sirupsen/logrus"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---------------------------------------------------------------------------
// Tool 1 – transfer_single_workspace
// ---------------------------------------------------------------------------

// TransferSingleWorkspace creates a tool that migrates one workspace to another
// organization / project using POST /api/v2/workspaces/:id/actions/transfer.
func TransferSingleWorkspace(logger *log.Logger) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("transfer_single_workspace",
			mcp.WithDescription(
				`Migrates a single Terraform workspace to another organization or project (cross-org / TFE→TFC).
Calls POST /api/v2/workspaces/:workspace_id/actions/transfer.
Returns a transfer_id that you can pass to get_workspace_transfer_summary to track progress.
This is a DESTRUCTIVE operation – the workspace ownership changes immediately.`),
			mcp.WithTitleAnnotation("Transfer a single workspace to another org/project"),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("workspace_id",
				mcp.Required(),
				mcp.Description("The ID of the workspace to transfer (e.g. ws-xxxxxxxxxxxxxxxx)"),
			),
			mcp.WithString("destination_organization_name",
				mcp.Required(),
				mcp.Description("The name of the destination Terraform organization"),
			),
			mcp.WithString("destination_project_name",
				mcp.Required(),
				mcp.Description("The name of the destination project inside the destination organization"),
			),
			mcp.WithString("destination_vcs_provider_id",
				mcp.Description("Optional VCS OAuth token ID in the destination org (e.g. ot-xxxxxxxx). Required when the workspace has a VCS connection."),
			),
			mcp.WithString("migrate_varset",
				mcp.Description("Whether to migrate variable sets along with the workspace: 'true' or 'false' (default: 'false')"),
			),
		),
		Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return transferSingleWorkspaceHandler(ctx, request, logger)
		},
	}
}

func transferSingleWorkspaceHandler(ctx context.Context, request mcp.CallToolRequest, logger *log.Logger) (*mcp.CallToolResult, error) {
	workspaceID, err := request.RequireString("workspace_id")
	if err != nil {
		return ToolError(logger, "missing required input: workspace_id", err)
	}
	workspaceID = strings.TrimSpace(workspaceID)

	destOrg, err := request.RequireString("destination_organization_name")
	if err != nil {
		return ToolError(logger, "missing required input: destination_organization_name", err)
	}

	destProject, err := request.RequireString("destination_project_name")
	if err != nil {
		return ToolError(logger, "missing required input: destination_project_name", err)
	}

	destVCSProviderID := strings.TrimSpace(request.GetString("destination_vcs_provider_id", ""))
	migrateVarsetStr := strings.ToLower(strings.TrimSpace(request.GetString("migrate_varset", "false")))
	migrateVarset := migrateVarsetStr == "true"

	mc, err := client.GetMigrateClientFromContext(ctx, logger)
	if err != nil {
		return ToolError(logger, "failed to get migrate client – ensure TFE_TOKEN and TFE_ADDRESS are configured", err)
	}

	req := client.SingleTransferRequest{
		DestinationOrganizationName: strings.TrimSpace(destOrg),
		DestinationProjectName:      strings.TrimSpace(destProject),
		DestinationVCSProviderID:    destVCSProviderID,
		MigrateVarset:               migrateVarset,
	}

	result, err := mc.TransferSingleWorkspace(ctx, workspaceID, req)
	if err != nil {
		return ToolError(logger, "failed to transfer workspace", err)
	}

	out := map[string]interface{}{
		"transfer_id":  result.TransferID,
		"message":      "Workspace transfer accepted. Use get_workspace_transfer_summary with the transfer_id to check progress.",
		"workspace_id": workspaceID,
		"destination": map[string]string{
			"organization": req.DestinationOrganizationName,
			"project":      req.DestinationProjectName,
		},
	}
	jsonBytes, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// ---------------------------------------------------------------------------
// Tool 2 – transfer_bulk_workspaces
// ---------------------------------------------------------------------------

// TransferBulkWorkspaces creates a tool that migrates multiple workspaces at
// once using POST /api/v2/workspaces/transfer.
func TransferBulkWorkspaces(logger *log.Logger) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("transfer_bulk_workspaces",
			mcp.WithDescription(
				`Bulk-migrates multiple Terraform workspaces from one organization to another.
Calls POST /api/v2/workspaces/transfer.
Accepts a JSON array of workspace mappings in the workspace_map parameter.
Returns a transfer_id to track progress via get_workspace_transfer_summary.
This is a DESTRUCTIVE operation.`),
			mcp.WithTitleAnnotation("Bulk-transfer multiple workspaces to another org"),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("source_organization_name",
				mcp.Required(),
				mcp.Description("The name of the source Terraform organization"),
			),
			mcp.WithString("destination_organization_name",
				mcp.Required(),
				mcp.Description("The name of the destination Terraform organization"),
			),
			mcp.WithString("workspace_map",
				mcp.Required(),
				mcp.Description(`JSON array describing each workspace to transfer. Each element must have:
  - source_workspace_name (string, required)
  - destination_project_name (string, required)
  - destination_vcs_provider_id (string, optional)
  - migrate_varset (boolean, optional, default false)

Example:
[
  {"source_workspace_name":"ws-dev","destination_project_name":"prod-project","migrate_varset":false},
  {"source_workspace_name":"ws-staging","destination_project_name":"prod-project","destination_vcs_provider_id":"ot-Axhakal","migrate_varset":true}
]`),
			),
		),
		Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return transferBulkWorkspacesHandler(ctx, request, logger)
		},
	}
}

func transferBulkWorkspacesHandler(ctx context.Context, request mcp.CallToolRequest, logger *log.Logger) (*mcp.CallToolResult, error) {
	srcOrg, err := request.RequireString("source_organization_name")
	if err != nil {
		return ToolError(logger, "missing required input: source_organization_name", err)
	}

	destOrg, err := request.RequireString("destination_organization_name")
	if err != nil {
		return ToolError(logger, "missing required input: destination_organization_name", err)
	}

	workspaceMapStr, err := request.RequireString("workspace_map")
	if err != nil {
		return ToolError(logger, "missing required input: workspace_map", err)
	}

	var workspaceMap []client.WorkspaceMapEntry
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(workspaceMapStr)), &workspaceMap); jsonErr != nil {
		return ToolError(logger, "workspace_map must be a valid JSON array", jsonErr)
	}
	if len(workspaceMap) == 0 {
		return ToolErrorf(logger, "workspace_map must contain at least one entry")
	}

	mc, err := client.GetMigrateClientFromContext(ctx, logger)
	if err != nil {
		return ToolError(logger, "failed to get migrate client – ensure TFE_TOKEN and TFE_ADDRESS are configured", err)
	}

	req := client.BulkTransferRequest{
		SourceOrganizationName:      strings.TrimSpace(srcOrg),
		DestinationOrganizationName: strings.TrimSpace(destOrg),
		WorkspaceMap:                workspaceMap,
	}

	result, err := mc.TransferBulkWorkspaces(ctx, req)
	if err != nil {
		return ToolError(logger, "failed to bulk-transfer workspaces", err)
	}

	out := map[string]interface{}{
		"transfer_id":     result.TransferID,
		"message":         "Bulk workspace transfer accepted. Use get_workspace_transfer_summary with the transfer_id to check progress.",
		"workspace_count": len(workspaceMap),
		"source_org":      req.SourceOrganizationName,
		"destination_org": req.DestinationOrganizationName,
	}
	jsonBytes, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// ---------------------------------------------------------------------------
// Tool 3 – get_workspace_transfer_summary
// ---------------------------------------------------------------------------

// GetWorkspaceTransferSummary creates a tool that retrieves the status of a
// previous transfer via GET /api/v2/workspaces/transfer/:transfer_id/summary.
func GetWorkspaceTransferSummary(logger *log.Logger) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_workspace_transfer_summary",
			mcp.WithDescription(
				`Retrieves the migration status for all workspaces in a transfer job.
Calls GET /api/v2/workspaces/transfer/:transfer_id/summary.
Each artifact field (varsets, vcs, state_files, plan_files, config_files) will be one of:
  NOT_REQUESTED  – migration was not requested for this artifact
  NOT_APPLICABLE – artifact does not apply to this workspace
  MIGRATED       – artifact was successfully migrated
  NOT_MIGRATED   – artifact migration has not completed yet
Use this after transfer_single_workspace or transfer_bulk_workspaces to monitor progress.`),
			mcp.WithTitleAnnotation("Get workspace transfer/migration summary"),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithString("transfer_id",
				mcp.Required(),
				mcp.Description("The transfer job ID returned by transfer_single_workspace or transfer_bulk_workspaces (e.g. tfr-xxxxxxxxxxxxxxxx)"),
			),
		),
		Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return getWorkspaceTransferSummaryHandler(ctx, request, logger)
		},
	}
}

func getWorkspaceTransferSummaryHandler(ctx context.Context, request mcp.CallToolRequest, logger *log.Logger) (*mcp.CallToolResult, error) {
	transferID, err := request.RequireString("transfer_id")
	if err != nil {
		return ToolError(logger, "missing required input: transfer_id", err)
	}
	transferID = strings.TrimSpace(transferID)

	mc, err := client.GetMigrateClientFromContext(ctx, logger)
	if err != nil {
		return ToolError(logger, "failed to get migrate client – ensure TFE_TOKEN and TFE_ADDRESS are configured", err)
	}

	rows, err := mc.GetTransferSummary(ctx, transferID)
	if err != nil {
		return ToolError(logger, "failed to retrieve transfer summary", err)
	}

	// Compute overall stats for quick LLM consumption
	stats := computeTransferStats(rows)
	out := map[string]interface{}{
		"transfer_id": transferID,
		"total":       len(rows),
		"summary":     stats,
		"workspaces":  rows,
	}

	jsonBytes, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// computeTransferStats tallies migration statuses across all workspace rows.
// Valid status values: NOT_REQUESTED, NOT_APPLICABLE, MIGRATED, NOT_MIGRATED.
func computeTransferStats(rows []client.TransferSummaryRow) map[string]interface{} {
	statusFields := []struct {
		label  string
		getter func(client.TransferSummaryRow) string
	}{
		{"varsets", func(r client.TransferSummaryRow) string { return r.VarsetsMigrationStatus }},
		{"vcs", func(r client.TransferSummaryRow) string { return r.VCSMigrationStatus }},
		{"state_files", func(r client.TransferSummaryRow) string { return r.StateFilesMigrationStatus }},
		{"plan_files", func(r client.TransferSummaryRow) string { return r.PlanFilesMigrationStatus }},
		{"config_files", func(r client.TransferSummaryRow) string { return r.ConfigFilesMigrationStatus }},
	}

	totals := make(map[string]map[string]int)
	for _, sf := range statusFields {
		totals[sf.label] = map[string]int{}
	}

	for _, row := range rows {
		for _, sf := range statusFields {
			status := sf.getter(row)
			totals[sf.label][status]++
		}
	}

	result := make(map[string]interface{}, len(statusFields))
	for k, v := range totals {
		result[k] = v
	}
	return result
}
