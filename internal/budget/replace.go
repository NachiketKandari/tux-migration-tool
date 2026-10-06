package budget

import (
	"fmt"
	"sort"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/ir"
)

// QueryReport is the per-query accounting of one replacement — the audit
// side of the rewrite (PRD §4.7): what was removed, what replaced it, and
// where both live.
type QueryReport struct {
	QueryID     string
	QueryType   ir.QueryType
	StartLine   int // 1-based source lines of the replaced region
	EndLine     int
	ViewLine    int // 1-based line of the call in the rewritten source
	LinesBefore int
	CharsBefore int
	CharsAfter  int
}

// View is the rewritten legacy source plus its accounting.
type View struct {
	Source string
	Report []QueryReport

	CharsBefore int // total chars of the replaced SQL regions
	CharsAfter  int // total chars of the replacing call lines
}

// ShrinkPct returns the char mass removed from the replaced regions.
func (v View) ShrinkPct() float64 {
	if v.CharsBefore == 0 {
		return 0
	}
	return (1 - float64(v.CharsAfter)/float64(v.CharsBefore)) * 100
}

// ReplaceQueries rewrites src — a whole .pc file or a branch slice — by
// replacing every query's [StartLine, EndLine] line range (1-based,
// inclusive, interpreted relative to src; flattened cursors already span
// DECLARE..CLOSE) with the single resolved call line, indented to the
// replaced first line's depth. Every query must resolve in calls and every
// call must carry a context name — a gap is an error, never a raw-SQL
// passthrough, since the controller prompt contract (§4.2.4) forbids
// leaking SQL. Overlapping ranges are errors.
func ReplaceQueries(src string, queries []*ir.Query, calls map[string]DBCall) (View, error) {
	ranges := make([]*ir.Query, len(queries))
	copy(ranges, queries)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].StartLine < ranges[j].StartLine })

	lines := strings.Split(src, "\n")
	for i, q := range ranges {
		if q.StartLine < 1 || q.EndLine < q.StartLine || q.EndLine > len(lines) {
			return View{}, fmt.Errorf("budget: query %q range %d-%d outside src (%d lines)",
				q.ID, q.StartLine, q.EndLine, len(lines))
		}
		if i > 0 && ranges[i-1].EndLine >= q.StartLine {
			return View{}, fmt.Errorf("budget: query %q range %d-%d overlaps %q (%d-%d)",
				q.ID, q.StartLine, q.EndLine, ranges[i-1].ID, ranges[i-1].StartLine, ranges[i-1].EndLine)
		}
		call, ok := calls[q.ID]
		if !ok {
			return View{}, fmt.Errorf("budget: no resolved DB call for query %q", q.ID)
		}
		if call.Name == "" {
			return View{}, fmt.Errorf("budget: empty DB call name for query %q", q.ID)
		}
		if call.CtxName == "" {
			return View{}, fmt.Errorf("budget: empty context name for query %q call %s", q.ID, call.Name)
		}
	}

	view := View{Report: make([]QueryReport, 0, len(ranges))}
	var out []string
	pos := 0 // 0-based index of the next uncopied line
	for _, q := range ranges {
		start, end := q.StartLine-1, q.EndLine // half-open for copy(out, lines)
		out = append(out, lines[pos:start]...)
		call := calls[q.ID]
		callLine := common.Leading(lines[start]) + call.Line()
		out = append(out, callLine)
		pos = end

		// Only the region's *length* is needed, so sum it
		// arithmetically instead of joining the lines into a
		// throwaway string: n lines joined by "\n" is
		// sum(len) + (n-1), and an empty region is 0.
		chars := 0
		if region := lines[start:end]; len(region) > 0 {
			for _, l := range region {
				chars += len(l) + 1
			}
			chars--
		}
		view.Report = append(view.Report, QueryReport{
			QueryID:     q.ID,
			QueryType:   q.Type,
			StartLine:   q.StartLine,
			EndLine:     q.EndLine,
			ViewLine:    len(out), // 1-based: the call was appended last
			LinesBefore: q.EndLine - q.StartLine + 1,
			CharsBefore: chars,
			CharsAfter:  len(callLine),
		})
		view.CharsBefore += chars
		view.CharsAfter += len(callLine)
	}
	out = append(out, lines[pos:]...)
	view.Source = strings.Join(out, "\n")
	return view, nil
}
