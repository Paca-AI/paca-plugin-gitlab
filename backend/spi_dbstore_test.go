package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

// fakeSQL is just enough SQL for the setup-session statements: single-row
// INSERT with a column list, and SELECT ... WHERE col = $1. A column an INSERT
// does not name reads back as NULL, so a field the store forgets to write is
// lost exactly as it would be in the real table. (The in-memory spiStore the
// provider tests use keeps every field and cannot notice that.)
type fakeSQL struct {
	tables map[string][]map[string]any
}

var (
	insertRe = regexp.MustCompile(`(?is)INSERT INTO (\w+)\s*\(([^)]*)\)\s*VALUES\s*\((.*)\)\s*$`)
	selectRe = regexp.MustCompile(`(?is)SELECT (.*?) FROM (\w+) WHERE (\w+) = \$1`)
)

func (f *fakeSQL) Exec(sql string, params ...any) (int64, error) {
	m := insertRe.FindStringSubmatch(sql)
	if m == nil {
		return 0, fmt.Errorf("fakeSQL: unsupported statement %q", strings.TrimSpace(sql))
	}
	cols := strings.Split(m[2], ",")
	vals := strings.Split(m[3], ",")
	if len(cols) != len(vals) {
		return 0, fmt.Errorf("fakeSQL: %d columns but %d values", len(cols), len(vals))
	}
	row := map[string]any{}
	for i, c := range cols {
		v := strings.TrimSpace(vals[i])
		switch {
		case strings.HasPrefix(v, "$"):
			n, err := strconv.Atoi(v[1:])
			if err != nil || n < 1 || n > len(params) {
				return 0, fmt.Errorf("fakeSQL: bad parameter %s", v)
			}
			row[strings.TrimSpace(c)] = params[n-1]
		case strings.HasPrefix(v, "'"):
			row[strings.TrimSpace(c)] = strings.Trim(v, "'")
		default:
			return 0, fmt.Errorf("fakeSQL: unsupported value %q", v)
		}
	}
	if f.tables == nil {
		f.tables = map[string][]map[string]any{}
	}
	f.tables[m[1]] = append(f.tables[m[1]], row)
	return 1, nil
}

func (f *fakeSQL) Query(sql string, params ...any) (*plugin.DBQueryResult, error) {
	m := selectRe.FindStringSubmatch(sql)
	if m == nil {
		return nil, fmt.Errorf("fakeSQL: unsupported query %q", strings.TrimSpace(sql))
	}
	var cols []string
	for _, c := range strings.Split(m[1], ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	out := &plugin.DBQueryResult{Columns: cols}
	for _, row := range f.tables[m[2]] {
		if fmt.Sprint(row[m[3]]) != fmt.Sprint(params[0]) {
			continue
		}
		r := make([]any, len(cols))
		for i, c := range cols {
			r[i] = row[c]
		}
		out.Rows = append(out.Rows, r)
	}
	return out, nil
}

func newTestDBStore() dbStore { return dbStore{p: &gitlabPlugin{}, db: &fakeSQL{}} }

// The setup session has to survive a round trip through the real store: the
// host decides where the token is validated, so losing it would validate the
// token against the wrong instance.
func TestDBStore_SetupSessionKeepsItsHostAndScope(t *testing.T) {
	s := newTestDBStore()
	in := setupSession{State: "st-1", Scope: "agent", AgentID: "agent-7", Host: "gitlab.example.com", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Millisecond)}
	if err := s.saveSetup(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadSetup("st-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "agent" || got.AgentID != "agent-7" || got.Host != "gitlab.example.com" || got.Completed || !got.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("round trip lost data: %+v", got)
	}
	if _, err := s.loadSetup("unknown"); err != errNoSuchSetup {
		t.Errorf("unknown state: %v", err)
	}
}
