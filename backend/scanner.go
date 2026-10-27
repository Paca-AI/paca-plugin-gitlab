package main

import "fmt"

// scanner provides convenient typed access to a row from [plugin.DBQueryResult].
type scanner struct {
	idx map[string]int
	row []any
}

func newRowScanner(cols []string, row []any) *scanner {
	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[c] = i
	}
	return &scanner{idx: idx, row: row}
}

func (s *scanner) str(col string) string {
	i, ok := s.idx[col]
	if !ok || i >= len(s.row) || s.row[i] == nil {
		return ""
	}
	if v, ok := s.row[i].(string); ok {
		return v
	}
	return fmt.Sprintf("%v", s.row[i])
}

func (s *scanner) strPtr(col string) *string {
	i, ok := s.idx[col]
	if !ok || i >= len(s.row) || s.row[i] == nil {
		return nil
	}
	v := s.str(col)
	return &v
}

func (s *scanner) int64Val(col string) int64 {
	i, ok := s.idx[col]
	if !ok || i >= len(s.row) || s.row[i] == nil {
		return 0
	}
	switch v := s.row[i].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
