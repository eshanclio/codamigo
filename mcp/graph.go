package mcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ieshan/codamigo/query"
)

// openWorldFalse backs the OpenWorldHint pointer: every codamigo tool answers
// from the local index and never reaches an open-ended external system.
var openWorldFalse = false

// readOnly returns the annotations shared by tools that only read the index.
// IdempotentHint is deliberately unset; the MCP schema defines it as meaningful
// only when ReadOnlyHint is false.
func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: &openWorldFalse,
	}
}

// maxGraphResults caps how many refs a single graph call returns, so a symbol
// referenced everywhere cannot flood the agent's context.
const maxGraphResults = 100

// GetCallersInput are the parameters for the get_callers tool.
type GetCallersInput struct {
	Symbol string `json:"symbol" jsonschema:"Exact name of the symbol to find references to, e.g. \"ReplaceByFiles\""`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of callers to return (default 50, max 100)"`
}

// GetCalleesInput are the parameters for the get_callees tool.
type GetCalleesInput struct {
	Symbol string `json:"symbol" jsonschema:"Exact name of the symbol whose references to list, e.g. \"Index\""`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of callees to return (default 50, max 100)"`
}

// GetImpactInput are the parameters for the get_impact tool.
type GetImpactInput struct {
	Symbol string `json:"symbol" jsonschema:"Exact name of the symbol whose change impact to assess"`
	Depth  int    `json:"depth,omitempty" jsonschema:"How many levels of callers to traverse (default 2, max 10)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of affected symbols to return (default 50, max 100)"`
}

func (s *Server) HandleGetCallers(ctx context.Context, _ *mcp.CallToolRequest, input GetCallersInput) (*mcp.CallToolResult, any, error) {
	symbol, bad := validateSymbol(input.Symbol)
	if bad != nil {
		return bad, nil, nil
	}
	if s.querier == nil {
		return newErrorResult("no querier configured"), nil, nil
	}

	refs, err := s.querier.Callers(ctx, symbol)
	if err != nil {
		return graphError(ctx, "get_callers", err), nil, nil
	}
	return newTextResult(query.FormatRefs(refs, symbol, query.RefFormat{
		Relation:      "callers of",
		Limit:         graphLimit(input.Limit),
		PreferRefSite: true,
	})), nil, nil
}

func (s *Server) HandleGetCallees(ctx context.Context, _ *mcp.CallToolRequest, input GetCalleesInput) (*mcp.CallToolResult, any, error) {
	symbol, bad := validateSymbol(input.Symbol)
	if bad != nil {
		return bad, nil, nil
	}
	if s.querier == nil {
		return newErrorResult("no querier configured"), nil, nil
	}

	refs, err := s.querier.Callees(ctx, symbol)
	if err != nil {
		return graphError(ctx, "get_callees", err), nil, nil
	}
	return newTextResult(query.FormatRefs(refs, symbol, query.RefFormat{
		Relation: "referenced by",
		Limit:    graphLimit(input.Limit),
	})), nil, nil
}

func (s *Server) HandleGetImpact(ctx context.Context, _ *mcp.CallToolRequest, input GetImpactInput) (*mcp.CallToolResult, any, error) {
	symbol, bad := validateSymbol(input.Symbol)
	if bad != nil {
		return bad, nil, nil
	}
	if s.querier == nil {
		return newErrorResult("no querier configured"), nil, nil
	}

	refs, err := s.querier.Impact(ctx, symbol, input.Depth)
	if err != nil {
		return graphError(ctx, "get_impact", err), nil, nil
	}
	return newTextResult(query.FormatRefs(refs, symbol, query.RefFormat{
		Relation:      "affected by changing",
		Limit:         graphLimit(input.Limit),
		ShowDepth:     true,
		PreferRefSite: true,
	})), nil, nil
}

// validateSymbol checks the symbol parameter, returning a ready error result
// when it is unusable.
func validateSymbol(symbol string) (string, *mcp.CallToolResult) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", newErrorResult("symbol is required")
	}
	// Symbol names are single identifiers; anything longer is a malformed query
	// rather than something to look up.
	if len(symbol) > 255 {
		return "", newErrorResult("symbol is too long")
	}
	return symbol, nil
}

// graphLimit clamps the requested result count.
func graphLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, maxGraphResults)
}

// graphError converts a query error into a client-safe result. ErrGraphNotBuilt
// is actionable by the agent, so it is surfaced; anything else is logged and
// reported generically so internal details never reach the client.
func graphError(ctx context.Context, tool string, err error) *mcp.CallToolResult {
	if errors.Is(err, query.ErrGraphNotBuilt) {
		return newTextResult("The code graph has not been built for this index yet. Run indexing (codamigo index) to enable caller and impact queries.")
	}
	slog.ErrorContext(ctx, "graph query failed", slog.String("tool", tool), slog.Any("error", err))
	return newErrorResult("graph query failed; check server logs")
}
