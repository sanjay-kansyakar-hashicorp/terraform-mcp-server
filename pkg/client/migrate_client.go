// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-mcp-server/version"
	log "github.com/sirupsen/logrus"
)

// -----------------------------------------------------------------------
// Migrate HTTP client store (keyed by session ID, same pattern as TFE)
// -----------------------------------------------------------------------

var activeMigrateClients sync.Map

// MigrateClient wraps the HTTP client together with the TFE address and
// bearer token so callers don't have to carry those values around.
type MigrateClient struct {
	httpClient *http.Client
	baseURL    string
	token      string
	logger     *log.Logger
}

// NewMigrateClient builds a MigrateClient for the given session.
func NewMigrateClient(sessionID, terraformAddress, terraformToken string, skipTLSVerify bool, logger *log.Logger) (*MigrateClient, error) {
	if terraformToken == "" {
		return nil, fmt.Errorf("no Terraform token provided for migrate client")
	}
	mc := &MigrateClient{
		httpClient: createHTTPClient(skipTLSVerify, logger),
		baseURL:    terraformAddress,
		token:      terraformToken,
		logger:     logger,
	}
	activeMigrateClients.Store(sessionID, mc)
	return mc, nil
}

// GetMigrateClient retrieves the cached MigrateClient for the session.
func GetMigrateClient(sessionID string) *MigrateClient {
	if v, ok := activeMigrateClients.Load(sessionID); ok {
		return v.(*MigrateClient)
	}
	return nil
}

// DeleteMigrateClient removes the cached MigrateClient for the session.
func DeleteMigrateClient(sessionID string) {
	activeMigrateClients.Delete(sessionID)
}

// GetMigrateClientFromContext is a convenience helper used by tool handlers.
func GetMigrateClientFromContext(ctx context.Context, logger *log.Logger) (*MigrateClient, error) {
	tfeClient, err := GetTfeClientFromContext(ctx, logger)
	if err != nil {
		return nil, err
	}
	_ = tfeClient // ensure TFE client exists (token is valid)

	// Reuse the same config values that the TFE client used.
	address := getAddressFromContext(ctx)
	token := getTokenFromContext(ctx)
	skipTLS := parseTerraformSkipTLSVerify(ctx)

	mc := &MigrateClient{
		httpClient: createHTTPClient(skipTLS, logger),
		baseURL:    address,
		token:      token,
		logger:     logger,
	}
	return mc, nil
}

// getAddressFromContext pulls TFE_ADDRESS from the request context (same logic
// as CreateTfeClientForSession, reproduced here to avoid import cycle).
func getAddressFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey(TerraformAddress)).(string); ok && v != "" {
		return v
	}
	addr := DefaultTerraformAddress
	return addr
}

func getTokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey(TerraformToken)).(string); ok && v != "" {
		return v
	}
	return ""
}

// -----------------------------------------------------------------------
// Request / Response types
// -----------------------------------------------------------------------

// WorkspaceMapEntry represents a single workspace mapping in a bulk transfer.
type WorkspaceMapEntry struct {
	SourceWorkspaceName      string `json:"source_workspace_name"`
	DestinationProjectName   string `json:"destination_project_name"`
	DestinationVCSProviderID string `json:"destination_vcs_provider_id,omitempty"`
	MigrateVarset            bool   `json:"migrate_varset"`
}

// SingleTransferRequest is the payload for POST /api/v2/workspaces/:id/actions/transfer
type SingleTransferRequest struct {
	DestinationOrganizationName string `json:"destination_organization_name"`
	DestinationProjectName      string `json:"destination_project_name"`
	DestinationVCSProviderID    string `json:"destination_vcs_provider_id,omitempty"`
	MigrateVarset               bool   `json:"migrate_varset"`
}

// BulkTransferRequest is the payload for POST /api/v2/workspaces/transfer
type BulkTransferRequest struct {
	DestinationOrganizationName string              `json:"destination_organization_name"`
	SourceOrganizationName      string              `json:"source_organization_name"`
	WorkspaceMap                []WorkspaceMapEntry `json:"workspace_map"`
}

// TransferResponse is the 202 body returned by both transfer endpoints.
type TransferResponse struct {
	TransferID string `json:"transfer_id"`
}

// TransferSummaryRow represents one CSV row from the summary endpoint, parsed
// into a struct for easier JSON marshalling back to the LLM.
type TransferSummaryRow struct {
	WorkspaceID                 string `json:"workspace_id"`
	DestinationWorkspaceName    string `json:"destination_workspace_name"`
	SourceOrganizationName      string `json:"source_organization_name"`
	SourceProjectName           string `json:"source_project_name"`
	DestinationOrganizationName string `json:"destination_organization_name"`
	DestinationProjectName      string `json:"destination_project_name"`
	VarsetsMigrationStatus      string `json:"varsets_migration_status"`
	VCSMigrationStatus          string `json:"vcs_migration_status"`
	StateFilesMigrationStatus   string `json:"state_files_migration_status"`
	PlanFilesMigrationStatus    string `json:"plan_files_migration_status"`
	ConfigFilesMigrationStatus  string `json:"config_files_migration_status"`
}

// -----------------------------------------------------------------------
// API methods
// -----------------------------------------------------------------------

// TransferSingleWorkspace calls POST /api/v2/workspaces/:workspace_id/actions/transfer
func (mc *MigrateClient) TransferSingleWorkspace(ctx context.Context, workspaceID string, req SingleTransferRequest) (*TransferResponse, error) {
	path := fmt.Sprintf("/api/v2/workspaces/%s/actions/transfer", workspaceID)
	return mc.doTransfer(ctx, path, req)
}

// TransferBulkWorkspaces calls POST /api/v2/workspaces/transfer
func (mc *MigrateClient) TransferBulkWorkspaces(ctx context.Context, req BulkTransferRequest) (*TransferResponse, error) {
	return mc.doTransfer(ctx, "/api/v2/workspaces/transfer", req)
}

// GetTransferSummary calls GET /api/v2/workspaces/transfer/:transfer_id/summary
// The API returns CSV; this method parses it and returns structured rows.
func (mc *MigrateClient) GetTransferSummary(ctx context.Context, transferID string) ([]TransferSummaryRow, error) {
	path := fmt.Sprintf("/api/v2/workspaces/transfer/%s/summary", transferID)
	url := mc.baseURL + path

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building GET request for transfer summary: %w", err)
	}
	mc.setCommonHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/csv")

	resp, err := mc.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling transfer summary API: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading transfer summary response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transfer summary API returned %d: %s", resp.StatusCode, string(body))
	}

	rows, err := parseTransferSummaryCSV(body)
	if err != nil {
		return nil, fmt.Errorf("parsing transfer summary CSV: %w", err)
	}
	return rows, nil
}

// -----------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------

func (mc *MigrateClient) doTransfer(ctx context.Context, path string, payload interface{}) (*TransferResponse, error) {
	url := mc.baseURL + path

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling transfer request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("building POST request: %w", err)
	}
	mc.setCommonHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := mc.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling transfer API at %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading transfer response body: %w", err)
	}

	if resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("transfer API at %s returned %d: %s", path, resp.StatusCode, string(body))
	}

	var result TransferResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshalling transfer response: %w", err)
	}
	return &result, nil
}

func (mc *MigrateClient) setCommonHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+mc.token)
	req.Header.Set("User-Agent", fmt.Sprintf("terraform-mcp-server/%s", version.GetHumanVersion()))
}

// parseTransferSummaryCSV parses a CSV/TSV response from the summary endpoint.
// Handles both comma and tab delimiters, and matches columns by header name.
func parseTransferSummaryCSV(data []byte) ([]TransferSummaryRow, error) {
	lines := splitLines(string(data))
	if len(lines) < 2 {
		return []TransferSummaryRow{}, nil
	}

	// Detect delimiter from header line
	headerLine := lines[0]
	var delim byte = ','
	if strings.Contains(headerLine, "\t") {
		delim = '\t'
	}

	headers := splitDelimited(headerLine, delim)
	colIdx := make(map[string]int)
	for i, h := range headers {
		colIdx[normalizeHeader(h)] = i
	}

	const (
		colWorkspaceID = "workspace id"
		colDestWSName  = "destination workspace name"
		colSrcOrg      = "source organization name"
		colSrcProject  = "source project name"
		colDestOrg     = "destination organization name"
		colDestProject = "destination project name"
		colVarsets     = "varsets migration status"
		colVCS         = "vcs migration status"
		colStateFiles  = "state files migration status"
		colPlanFiles   = "plan files migration status"
		colConfigFiles = "configuration files migration status"
	)

	get := func(cols []string, col string) string {
		i, ok := colIdx[col]
		if !ok || i >= len(cols) {
			return ""
		}
		return strings.TrimSpace(cols[i])
	}

	var rows []TransferSummaryRow
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		cols := splitDelimited(line, delim)
		if len(cols) < len(headers) {
			continue
		}
		rows = append(rows, TransferSummaryRow{
			WorkspaceID:                 get(cols, colWorkspaceID),
			DestinationWorkspaceName:    get(cols, colDestWSName),
			SourceOrganizationName:      get(cols, colSrcOrg),
			SourceProjectName:           get(cols, colSrcProject),
			DestinationOrganizationName: get(cols, colDestOrg),
			DestinationProjectName:      get(cols, colDestProject),
			VarsetsMigrationStatus:      get(cols, colVarsets),
			VCSMigrationStatus:          get(cols, colVCS),
			StateFilesMigrationStatus:   get(cols, colStateFiles),
			PlanFilesMigrationStatus:    get(cols, colPlanFiles),
			ConfigFilesMigrationStatus:  get(cols, colConfigFiles),
		})
	}
	return rows, nil
}

// Helper: normalize header for matching (lowercase, trim spaces/quotes/BOM)
func normalizeHeader(s string) string {
	s = strings.TrimPrefix(s, "\xef\xbb\xbf")
	s = strings.Trim(s, "\r\"\t ")
	return strings.ToLower(strings.TrimSpace(s))
}

// Helper: split line by delimiter, respecting quoted fields
func splitDelimited(line string, delim byte) []string {
	var fields []string
	inQuote := false
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
		case ch == delim && !inQuote:
			fields = append(fields, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	fields = append(fields, strings.TrimSpace(cur.String()))
	return fields
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func splitTSV(line string) []string {
	var cols []string
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\t' {
			cols = append(cols, line[start:i])
			start = i + 1
		}
	}
	cols = append(cols, line[start:])
	return cols
}
