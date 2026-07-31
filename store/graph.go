package store

import (
	"context"
	"fmt"
)

// graphBatchSize bounds how many bind parameters a single graph query uses,
// matching the batching already applied in EmbeddingsByContentHash.
const graphBatchSize = 500

func (s *sqliteStore) EdgesBySource(ctx context.Context, srcIDs []string) ([]Edge, error) {
	if len(srcIDs) == 0 {
		return nil, nil
	}

	var edges []Edge
	for start := 0; start < len(srcIDs); start += graphBatchSize {
		end := min(start+graphBatchSize, len(srcIDs))
		batch := srcIDs[start:end]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		query := fmt.Sprintf(
			`SELECT src_id, file_path, src_name, kind, dst_name, dst_qualifier, line
			 FROM edges WHERE src_id IN (%s) ORDER BY line`, placeholders(len(batch)))

		batchEdges, err := s.scanEdges(ctx, query, args)
		if err != nil {
			return nil, fmt.Errorf("querying edges by source: %w", err)
		}
		edges = append(edges, batchEdges...)
	}
	return edges, nil
}

func (s *sqliteStore) EdgesByTargetName(ctx context.Context, name string) ([]Edge, error) {
	if name == "" {
		return nil, nil
	}
	edges, err := s.scanEdges(ctx,
		`SELECT src_id, file_path, src_name, kind, dst_name, dst_qualifier, line
		 FROM edges WHERE dst_name = ? ORDER BY file_path, line`, []any{name})
	if err != nil {
		return nil, fmt.Errorf("querying edges by target name: %w", err)
	}
	return edges, nil
}

// scanEdges runs an edge query and materialises the rows.
func (s *sqliteStore) scanEdges(ctx context.Context, query string, args []any) ([]Edge, error) {
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.SrcID, &e.FilePath, &e.SrcName, &e.Kind, &e.DstName, &e.DstQualifier, &e.Line); err != nil {
			return nil, fmt.Errorf("scanning edge: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating edges: %w", err)
	}
	return edges, nil
}

func (s *sqliteStore) ImportsByFile(ctx context.Context, filePaths []string) ([]Import, error) {
	if len(filePaths) == 0 {
		return nil, nil
	}

	var imports []Import
	for start := 0; start < len(filePaths); start += graphBatchSize {
		end := min(start+graphBatchSize, len(filePaths))
		batch := filePaths[start:end]

		args := make([]any, len(batch))
		for i, p := range batch {
			args[i] = p
		}
		query := fmt.Sprintf(
			`SELECT file_path, module, alias, line
			 FROM file_imports WHERE file_path IN (%s) ORDER BY file_path, line`, placeholders(len(batch)))

		batchImports, err := s.scanImports(ctx, query, args)
		if err != nil {
			return nil, fmt.Errorf("querying imports by file: %w", err)
		}
		imports = append(imports, batchImports...)
	}
	return imports, nil
}

// scanImports runs an import query and materialises the rows. It exists so each
// batch's rows are closed by defer rather than by hand at every return.
func (s *sqliteStore) scanImports(ctx context.Context, query string, args []any) ([]Import, error) {
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var imports []Import
	for rows.Next() {
		var im Import
		if err := rows.Scan(&im.FilePath, &im.Module, &im.Alias, &im.Line); err != nil {
			return nil, fmt.Errorf("scanning import: %w", err)
		}
		imports = append(imports, im)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating imports: %w", err)
	}
	return imports, nil
}

func (s *sqliteStore) EdgeCount(ctx context.Context) (int, error) {
	var count int
	if err := s.reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM edges").Scan(&count); err != nil {
		return 0, fmt.Errorf("counting edges: %w", err)
	}
	return count, nil
}
