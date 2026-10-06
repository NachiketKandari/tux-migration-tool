// Package gen is the deterministic generation core (PRD §4.2, plan-conversion
// §5.4): plan + IR + templates → real Go code. Models, DB methods, and every
// interface/handler/router artifact render from templates with zero LLM
// calls — the SQL is already known and the shapes are fixed by the FML ops.
// The LLM fills exactly one gap per endpoint: the controller body (Part 2b).
package gen

import (
	"fmt"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/goast"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/namer"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/profile"
	"tux-migration-tool/internal/sqltext"
	"tux-migration-tool/internal/templates"
	tsscan "tux-migration-tool/internal/tsscan"
)

// Options carries the generation inputs: the plan (units, naming pins), the
// main IR, and the fn-file IRs backing external-fn units.
type Options struct {
	Plan    *plan.Plan
	Main    *ir.File
	FnFiles []*ir.File
	// Source is the entry file's text — required only when the mapping
	// references discovery candidates (conditionRef): their conditions are
	// re-derived from the flow tree (PRD-2026-09-10 endpoint discovery).
	Source string
	// WithGorm renders the store with the legacy *gorm.DB handle alongside
	// sqlx (db.withGorm); default is the plain sqlx-only store.
	WithGorm bool
	// Templates is the template provider (user overlay over the embedded
	// set); nil = the embedded defaults.
	Templates templates.Provider
}

// Service bundles the derived naming context shared by all generators.
type Service struct {
	Mapping     *plan.Mapping
	Main        *ir.File
	ModelsPkg   string                    // root-folder/pkg/services/item/models
	Module      string                    // root-folder
	If          string                    // Item — exported service name for interface names
	WithGorm    bool                      // store carries the legacy gorm handle (db.withGorm)
	structLower string                    // itemController / itemHandler receiver base
	queries     map[string]*ir.Query      // namespaced ID → query (main + fn files)
	source      string                    // entry source text (conditionRef/scenarioRef resolution)
	flowTree    *flow.Tree                // lazily built when a ref endpoint appears
	scenMemo    map[string]*flow.Scenario // endpoint name → resolved scenario slice
	prof        profile.Profile           // target conventions (nil = goitem default)
	tpl         templates.Provider        // template set (nil-safe: always set in NewService)

	// scanFacts memoizes the scanner pass over source, and fnTrees the
	// per-helper flow trees derived from it. The entry's treeFor scans on its
	// own; this is one further scan shared by every helper, so a file with N
	// helpers costs two passes, not N+1.
	scanFacts *tsscan.SourceFacts
	fnTrees   map[string]*flow.Tree

	// srcLines memoizes strings.Split(source, "\n"). Four call sites slice
	// one branch's lines out of the whole entry source per endpoint, and a
	// 20k-line .pc makes each of those splits a fresh ~320KB []string —
	// hundreds of megabytes of garbage per service for text that never
	// changes. Same lazy-memo discipline as scanFacts above.
	srcLines []string
}

// sourceLines returns the entry source split on newlines, computed once.
// Callers must treat the result as read-only: it is shared.
func (s *Service) sourceLines() []string {
	if s.srcLines == nil {
		s.srcLines = strings.Split(s.source, "\n")
	}
	return s.srcLines
}

// helperFlowTree returns the flow tree of a fn helper's legacy body, or nil
// when the source is unavailable or the helper is not in it.
//
// The entry has treeFor; this is its per-function sibling. flow.Build already
// accepts any function name and pickFunction resolves it from the scanner's
// function table, so nothing new is needed from the parse stack — what was
// missing was a caller that asked for a helper rather than the entry.
func (s *Service) helperFlowTree(fn string) *flow.Tree {
	if strings.TrimSpace(s.source) == "" || fn == "" {
		return nil
	}
	if t, ok := s.fnTrees[fn]; ok {
		return t
	}
	if s.fnTrees == nil {
		s.fnTrees = map[string]*flow.Tree{}
	}
	if s.scanFacts == nil {
		facts, err := flow.ScanForIR(s.source, s.Main)
		if err != nil {
			// Memoize the miss so a bad source is diagnosed once rather
			// than once per helper.
			s.fnTrees[fn] = nil
			s.scanFacts = &tsscan.SourceFacts{}
			return nil
		}
		s.scanFacts = facts
	}
	s.fnTrees[fn] = flow.Build([]byte(s.source), s.scanFacts, fn, s.Main)
	return s.fnTrees[fn]
}

// Profile returns the service's target profile; the goitem default when
// unset (the zero-config default — P1: naming conventions read from the
// profile).
func (s *Service) Profile() profile.Profile {
	if s.prof == nil {
		return profile.Default()
	}
	return s.prof
}

// NewService derives the naming context from the plan and IR.
//
// Query indexing (severity F1): raw IDs belong to the main file alone.
// Referenced fn files are namespaced `fn:<name>:<qID>` — exactly the plan's
// pin namespace — and every other FnFile is scoped under its own base name,
// so an unreferenced file's `q1` can never silently overwrite the main's
// `q1`. Cross-file definitions sharing one ID with different SQL are a hard
// error, never a walk-order-dependent overwrite.
func NewService(o Options) (*Service, error) {
	if o.Plan == nil || o.Main == nil {
		return nil, fmt.Errorf("gen: plan and main IR are required")
	}
	s := &Service{Mapping: o.Plan.Mapping, Main: o.Main, WithGorm: o.WithGorm, source: o.Source}
	s.tpl = o.Templates
	if s.tpl == nil {
		s.tpl = templates.NewEmbeddedProvider()
	}
	s.ModelsPkg = s.Mapping.ImportPath("models")
	s.Module = strings.SplitN(s.Mapping.Module, "/", 2)[0]
	s.If = common.Export(s.Mapping.Service)
	s.structLower = s.Mapping.Service
	s.queries = make(map[string]*ir.Query, len(o.Main.Queries))
	origin := map[string]string{} // query id → defining file path
	index := func(id string, q *ir.Query, path string) error {
		if prev, dup := s.queries[id]; dup {
			if prev.SQL == q.SQL {
				return nil // same SQL under one id — benign re-index
			}
			return fmt.Errorf("gen: query %q defined by both %s and %s with different SQL — cross-file collision (severity F1 guard)", id, origin[id], path)
		}
		s.queries[id] = q
		origin[id] = path
		return nil
	}
	for _, q := range o.Main.Queries {
		if err := index(q.ID, q, o.Main.Path); err != nil {
			return nil, err
		}
	}

	fnIRByPath := make(map[string]*ir.File, len(o.FnFiles))
	handled := map[string]bool{o.Main.Path: true}
	for _, f := range o.FnFiles {
		fnIRByPath[f.Path] = f
	}
	for _, ext := range o.Main.ExternalFns {
		if strings.HasPrefix(ext.Name, "chk_") {
			continue // dropped construct at plan time (§4.8.4.1)
		}
		f := fnIRByPath[ext.DefinedIn]
		if f == nil {
			continue // unresolved fn — plan already blocks its endpoints
		}
		handled[f.Path] = true
		for _, q := range f.Queries {
			if err := index(ext.Name+":"+q.ID, q, f.Path); err != nil {
				return nil, err
			}
		}
	}
	for _, f := range o.FnFiles {
		if handled[f.Path] {
			continue
		}
		// Unreferenced file — scoped, inert, still collision-guarded.
		base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		for _, q := range f.Queries {
			if err := index(base+":"+q.ID, q, f.Path); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// Query resolves a namespaced query ID: main-file IDs as-is, referenced fn
// queries as "<fn>:<id>", other files' queries as "<file base>:<id>".
func (s *Service) Query(id string) *ir.Query { return s.queries[id] }

// Pin returns the user's method pin for a query ID, if any.
func (s *Service) Pin(queryID string) (plan.MethodPin, bool) {
	pin, ok := s.Mapping.DBMethods[queryID]
	return pin, ok
}

// lowerFirst renders itemController/itemHandler-style receiver type names.
// fieldFromFML derives a Go field name from an FML field name:
// FML_COMP_CD → CompCd (deterministic; reference prettiness is a §4.8.5 note).
func fieldFromFML(fml string) string {
	return common.FieldFromFML(fml)
}

// camelLower renders snake_case host var names as lowerCamelCase Go names.
// requestType / responseType / rowType naming conventions — delegated to
// the target profile (P1: goitem renders today's conventions verbatim, so
// goldens hold; a second profile renders its own).
func (s *Service) requestType(endpoint string) string { return s.Profile().Naming().Request(endpoint) }
func (s *Service) responseType(endpoint string) string {
	return s.Profile().Naming().Response(endpoint)
}

// RowName derives the models struct carrying one query's result row: the
// mapping pin's Row when set, else the method name minus its verb. The verb
// list mirrors plan.methodName's emitted prefixes (Get/Insert/Update/Delete/
// Merge — A2.6 added Merge, which plan emits for unpinned MERGE units). A
// digit-leading result takes the common X escape — never "_", which would
// leave the row type unexported and break db/controller references.
func (s *Service) RowName(queryID, methodName string) string {
	if pin, ok := s.Pin(queryID); ok && pin.Row != "" {
		return common.EscapeLeadingDigit(pin.Row)
	}
	return common.EscapeLeadingDigit(s.Profile().Naming().Row(methodName))
}

// FnRowContracts renders the row struct definitions a fn helper's store
// calls return — verbatim Go, the never-invent-fields contract the
// controller prompt enforces (the model guesses field names when the
// struct isn't quoted). Queries without a row shape (COUNT/DML) contribute
// nothing.
func (s *Service) FnRowContracts(p *plan.Plan, queryIDs []string) string {
	dbUnitByQuery := map[string]plan.Unit{}
	for _, u := range p.Units {
		if u.Kind == plan.KindDBMethod && len(u.QueryIDs) > 0 {
			dbUnitByQuery[u.QueryIDs[0]] = u
		}
	}
	var sb strings.Builder
	seen := map[string]bool{}
	for _, id := range queryIDs {
		orig := s.Query(id)
		if orig == nil {
			continue
		}
		canon := orig
		canonKey := id
		if orig.DuplicateOf != "" {
			canonKey = canonicalAliasID(id, orig)
			if cq := s.Query(canonKey); cq != nil {
				canon = cq
			} else if cq := s.Query(orig.DuplicateOf); cq != nil {
				canon = cq
				canonKey = orig.DuplicateOf
			} else {
				continue
			}
		}
		q := canon
		if (q.Type != ir.QuerySelectSingle && q.Type != ir.QuerySelectMulti) || isCountQuery(q) {
			continue
		}
		fields, err := s.rowFields(canonKey, q)
		if err != nil {
			continue // the db-method renderer owns the loud failure
		}
		name := s.RowName(q.ID, dbUnitByQuery[q.ID].Name)
		if seen[name] {
			continue
		}
		seen[name] = true
		fmt.Fprintf(&sb, "type %s struct {\n", name)
		for _, f := range fields {
			fmt.Fprintf(&sb, "\t%s %s `db:\"%s\"`\n", f.Name, f.Type, f.DBTag)
		}
		sb.WriteString("}\n")
	}
	return sb.String()
}

// rowFields derives the row struct's fields from the FETCH-INTO host vars.
// The db tag is the sanctioned computed-column alias when the select item at
// the same position is computed (columnAliases — what Oracle returns for the
// aliased column); otherwise the Pro*C table-header convention names host
// vars after their columns (`sql_demo_comp_cd` → DEMO_COMP_CD), which is
// exactly what Oracle returns for unaliased bare-column selects — so the tag
// is the uppercased, sql_-stripped host var. Go field Names stay
// host-var-derived in both cases, so staged controllers (row.StrDesc.String)
// and testgen's field inventory stay byte-stable; only the db tag changes.
// Field formats are uniformly sql.NullString.
func (s *Service) rowFields(queryKey string, q *ir.Query) ([]templates.FieldSpec, error) {
	aliases, _ := s.columnAliases(queryKey, q)
	fields := make([]templates.FieldSpec, 0, len(q.RowShape))
	for i, hvName := range q.RowShape {
		// Pro*C host vars arrive qualified and indicator-annotated
		// ("ST_GST.D_CGST_AMT", "sql_x INDICATOR") — the field names from
		// the bare column token only (last dot segment, first token).
		if j := strings.LastIndex(hvName, "."); j >= 0 {
			hvName = hvName[j+1:]
		}
		if j := strings.IndexAny(hvName, " \t"); j > 0 {
			hvName = hvName[:j]
		}
		spec := templates.FieldSpec{}
		if alias, ok := aliases[i]; ok {
			spec.DBTag = alias
		} else {
			spec.DBTag = strings.ToUpper(strings.TrimPrefix(hvName, "sql_"))
		}
		// Names and types delegate to the Go namer over the contract
		// field (uniform-ir plan §3.3/Phase 4): row names are the
		// host-var-derived export, row types the uniform sql.NullString
		// rule (user directive, 2026-09-07 — no per-type guessing;
		// conversions happen in the controller layer). Golden-neutral:
		// proven identical on all pinned IR row shapes.
		spec.Name = namer.GoNamer{}.Prop(hvName)
		spec.Type = namer.GoNamer{}.FieldType(contract.Field{Kind: contract.FieldRow})
		fields = append(fields, spec)
	}
	if len(fields) == 0 {
		// RowShape empty means the INTO span captured no host refs — usually
		// a scan miss (e.g. spaced ": var" host refs), never a real shape.
		return nil, fmt.Errorf("gen: query %s has no row shape — the INTO span captured no host refs (check the source's host-ref formatting)", q.ID)
	}
	return fields, nil
}

// requestFields / responseFields derive the endpoint's contract structs from
// the branch's FML ops (decision 10: Fget32 = inputs, Fadd32 = outputs;
// dropped session/error plumbing never reaches models, §4.8.4). Error ops
// (PRD-2026-09-10 discovery) are error emissions — "fadd err = returning
// error" — never contract fields.
func contractFields(ops []ir.FmlOp, kind ir.FmlOpKind) []templates.FieldSpec {
	fields := make([]templates.FieldSpec, 0, len(ops))
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Dropped || op.Error || op.Kind != kind || seen[op.Field] {
			continue
		}
		seen[op.Field] = true
		spec := templates.FieldSpec{
			Name:    fieldFromFML(op.Field),
			Type:    "string",
			JSONTag: op.Field,
		}
		if kind == ir.FmlGet {
			if !op.Optional {
				spec.Binding = "required"
			}
		} else {
			spec.OmitEmpty = true
		}
		fields = append(fields, spec)
	}
	return fields
}

// ModelFile derives the models.go content: per-endpoint request/response
// structs plus every DB method's row struct.
func (s *Service) ModelFile(p *plan.Plan) (string, error) {
	data := templates.ModelFileData{Package: "models"}
	endpoints := map[string]bool{}
	for _, e := range s.Mapping.Endpoints {
		endpoints[e.Name] = true
	}
	for _, e := range s.Mapping.Endpoints {
		c := s.conditionOf(e)
		if c == nil {
			continue
		}
		data.Structs = append(data.Structs, templates.StructSpec{
			Name:   s.requestType(e.Name),
			Fields: s.requestFields(e, c),
		})
		data.Structs = append(data.Structs, templates.StructSpec{
			Name:   s.responseType(e.Name),
			Fields: contractFields(c.FmlOps, ir.FmlAdd),
		})
	}
	// One struct per row name: identical shapes dedup; a name pinned for two
	// different shapes is a loud mapping error — emitting both declares the
	// type twice, which only surfaces where a target module compiles
	// (syntax-only runs would stage broken code silently).
	rowSig := map[string]string{}
	rowOwner := map[string]string{}
	addRow := func(u plan.Unit, q *ir.Query, hard bool) error {
		fields, err := s.rowFields(u.QueryIDs[0], q)
		if err != nil {
			if hard {
				// Host vars declared in Pro*C table headers may leave the
				// row shape unnamed; the pipeline flags it instead of guessing.
				return fmt.Errorf("gen: %w", err)
			}
			return nil // the db-method renderer owns the loud failure
		}
		name := s.RowName(u.QueryIDs[0], u.Name)
		sig := rowShapeSig(fields)
		if prev, ok := rowSig[name]; ok {
			if prev != sig {
				return fmt.Errorf("gen: row struct name %q is pinned by both %s and %s with different shapes — rename one row: pin in the mapping",
					name, rowOwner[name], u.QueryIDs[0])
			}
			return nil
		}
		rowSig[name] = sig
		rowOwner[name] = u.QueryIDs[0]
		data.Structs = append(data.Structs, templates.StructSpec{Name: name, Fields: fields})
		return nil
	}
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil {
			continue
		}
		switch {
		case q.Type == ir.QuerySelectMulti:
			if err := addRow(u, q, false); err != nil {
				return "", err
			}
		case q.Type == ir.QuerySelectSingle && !isCountQuery(q):
			if err := addRow(u, q, true); err != nil {
				return "", err
			}
		}
	}
	return s.render(templates.ModelFile, data)
}

// rowShapeSig fingerprints a row struct's fields (name/type/db tag) so
// identical shapes can share one declaration while a conflicting re-use of
// the same name is caught deterministically.
func rowShapeSig(fields []templates.FieldSpec) string {
	var sb strings.Builder
	for _, f := range fields {
		sb.WriteString(f.Name)
		sb.WriteByte(':')
		sb.WriteString(f.Type)
		sb.WriteByte(':')
		sb.WriteString(f.DBTag)
		sb.WriteByte(';')
	}
	return sb.String()
}

// FMLRequestMap renders the endpoint's FML-field → request-Go-field map
// from the same FmlGet derivation the request struct uses (§4.8.3): the
// deterministic input seam the controller view's Fget unpack rewrite
// consumes ("FML_USR_ID → UsrId"). Ops the request struct skips (error
// emissions, response adds) are absent.
func (s *Service) FMLRequestMap(endpoint string, c *ir.Condition) map[string]string {
	var e *plan.Endpoint
	for i := range s.Mapping.Endpoints {
		if s.Mapping.Endpoints[i].Name == endpoint {
			e = &s.Mapping.Endpoints[i]
			break
		}
	}
	if e == nil || c == nil {
		return nil
	}
	out := map[string]string{}
	for _, f := range s.requestFields(*e, c) {
		if f.JSONTag != "" {
			out[f.JSONTag] = f.Name
		}
	}
	return out
}

// requestFields derives one endpoint's request struct fields (§4.8.3):
// the branch's FmlGet ops, unioned with the entry-preamble Fgets whose
// host vars the branch body actually consumes (live-run fix, 2026-09-10 —
// the merge fixture reads FML_ACC_ID before the if/else; the
// branch-scoped IR carried no get for it, the request struct rendered
// empty, and the model had nothing to map). A preamble op whose target
// host var appears in the branch's source lines joins the contract;
// unused preamble reads (item's FML_MODE_FLG against flag-dispatched
// branches) stay out, so golden structs are unchanged.
func (s *Service) requestFields(e plan.Endpoint, c *ir.Condition) []templates.FieldSpec {
	fields := contractFields(c.FmlOps, ir.FmlGet)
	have := map[string]bool{}
	for _, f := range fields {
		have[f.JSONTag] = true
	}
	branchSrc := ""
	if s.source != "" && c.EndLine > c.StartLine {
		lines := s.sourceLines()
		from, to := c.StartLine, c.EndLine
		if to > len(lines) {
			to = len(lines)
		}
		if from >= 1 && from <= to {
			branchSrc = strings.Join(lines[from-1:to], "\n")
		}
	}
	if branchSrc == "" {
		return fields
	}
	var preamble []ir.FmlOp
	for _, op := range s.Main.FmlOps {
		if op.Kind != ir.FmlGet || op.Dropped || have[op.Field] {
			continue
		}
		if op.Target != "" && strings.Contains(branchSrc, op.Target) {
			preamble = append(preamble, op)
			have[op.Field] = true
		}
	}
	if len(preamble) == 0 {
		return fields
	}
	return append(fields, contractFields(preamble, ir.FmlGet)...)
}

func (s *Service) conditionOf(e plan.Endpoint) *ir.Condition {
	switch {
	case e.ScenarioRef != "", e.ScenarioFilter != "":
		// Dispatch-axis scenario (PRD-2026-09-12) or scenario filter
		// (scenario-filter plan §5): re-derive the fold from the flow tree
		// and synthesize the condition (the shared
		// flow.ScenarioCondition derivation). Degrade to nil (the caller
		// skips the endpoint's structs) on any failure — the plan already
		// validated the ref, so this is defensive only.
		sc := s.scenarioOf(e)
		if sc == nil {
			return nil
		}
		t := s.treeFor()
		if t == nil {
			return nil
		}
		return flow.ScenarioCondition(sc, t)
	case e.ConditionRef != "":
		// Discovery candidate (PRD-2026-09-10): re-derive the synthesized
		// condition from the flow tree (the shared flow.TreeFor derivation).
		// Degrade to nil (caller skips the endpoint's structs) on any
		// failure — the plan already validated the ref, so this is
		// defensive only.
		t := s.treeFor()
		if t == nil {
			return nil
		}
		c, err := flow.ConditionFor(t, s.Main.Conditions, e.ConditionRef)
		if err != nil {
			return nil
		}
		return c
	}
	return s.Main.Condition(e.Condition)
}

// isCountQuery reports whether a SELECT single's select list is exactly a
// scalar COUNT — DECODE(COUNT(*),…) wrappers return strings, not counts.
func isCountQuery(q *ir.Query) bool {
	head := strings.TrimSpace(q.SQL)
	head = strings.TrimPrefix(strings.TrimPrefix(head, "SELECT"), "select")
	return strings.HasPrefix(strings.TrimSpace(head), "COUNT(")
}

// DBMethod derives one store method: signature, rendered body, and the
// interface signature line — all from the query descriptor + pin (zero LLM).
func (s *Service) DBMethod(u plan.Unit) (body, signature string, needsSQL bool, err error) {
	if len(u.QueryIDs) == 0 {
		return "", "", false, fmt.Errorf("gen: db unit %s has no query", u.ID)
	}
	q := s.Query(u.QueryIDs[0])
	if q == nil {
		return "", "", false, fmt.Errorf("gen: db unit %s references unknown query %q", u.ID, u.QueryIDs[0])
	}
	pin, _ := s.Pin(u.QueryIDs[0])
	errOnly := false

	params, err := s.dbParams(q, pin)
	if err != nil {
		return "", "", false, err
	}
	// BP-8 at the render site: the host-var INTO list is Pro*C, not SQL —
	// the IR keeps it in q.SQL (goldens pin the IR), the row struct scans
	// by column name, so the emitted query string drops the clause.
	sql := q.SQL
	switch q.Type {
	case ir.QuerySelectSingle, ir.QuerySelectMulti:
		sql = sqltext.StripInto(sql)
	}
	sql = sqltext.CollapseBinds(sql)
	// Deterministic computed-column aliases (sanctioned `AS` per computed
	// select item): Oracle names unaliased computed columns by their
	// expression text, so the row scan needs the alias — and the row
	// struct's db tag carries the same name (rowFields). Alignment-guarded
	// inside columnAliases (positional INTO): a mismatch skips injection
	// loudly via AliasReport, never silently.
	if aliases, _ := s.columnAliases(u.QueryIDs[0], q); len(aliases) > 0 {
		switch q.Type {
		case ir.QuerySelectSingle, ir.QuerySelectMulti:
			sql = sqltext.InjectAliases(sql, aliases)
		}
	}
	sql = sqltext.Format(sql)
	d := templates.DBMethodData{
		Receiver:  "g",
		StoreType: "store",
		Name:      u.Name,
		CtxName:   "c",
		Params:    params,
		Query:     strings.TrimRight(sql, " \t\n;"),
	}
	needsSQL = q.Type == ir.QuerySelectSingle

	switch q.Type {
	case ir.QuerySelectMulti:
		row := s.RowName(u.QueryIDs[0], u.Name)
		d.Multi, d.RowType = true, "models."+row
		d.VarName = common.LowerFirst(row)
		if !strings.HasSuffix(d.VarName, "s") {
			d.VarName += "s"
		}
	case ir.QuerySelectSingle:
		if isCountQuery(q) {
			d.Scalar, d.VarName = "int64", "count"
		} else {
			row := s.RowName(u.QueryIDs[0], u.Name)
			d.RowType, d.VarName = "models."+row, common.LowerFirst(row)
		}
	case ir.QueryInsert, ir.QueryUpdate, ir.QueryDelete, ir.QueryMerge:
		// DML contract (severity F2): INSERT/UPDATE/DELETE render through
		// the tx-variant templates when the owning scenarios prove a live
		// begin→commit span (G-SCEN6, the scenario tx vote on u.Tx), and
		// through the plain autocommit template when none does — the
		// standalone-DML downgrade the IR's template table reserves. MERGE
		// keeps db_method_merge either way. All are error-only: ExecContext
		// + RowsAffected (the delete variant logs and returns without a
		// RowsAffected check).
		errOnly = true
		d.TxParam = q.Type != ir.QueryMerge && u.Tx
		d.SuccessMsg = u.Name + " executed successfully"
	default:
		return "", "", false, fmt.Errorf("gen: query %s type %s not supported by the deterministic db generator yet", q.ID, q.Type)
	}

	// Template resolution honors the IR's per-type choice (q.TemplateID is
	// passed through the plan as u.TemplateID); when the plan's ID is absent
	// or disagrees with the query type, the type's canonical template wins —
	// never a cross-type fallback. Non-tx DML (no owning scenario proves a
	// live begin→commit span) downgrades to the plain autocommit template;
	// MERGE keeps its own (severity F2).
	tmpl := templates.ID(u.TemplateID)
	if want := templates.ID(q.Type.TemplateID()); want != "" && tmpl != want {
		tmpl = want
	}
	if q.Type.IsDML() && q.Type != ir.QueryMerge && !u.Tx {
		tmpl = templates.DBMethodDMLPlain
	}
	if tmpl == "" {
		tmpl = templates.DBMethodSelectMulti
	}
	body, err = s.render(tmpl, d)
	if err != nil {
		return "", "", false, err
	}
	sig := u.Name + "(c context.Context"
	if d.TxParam {
		sig += ", tx *sqlx.Tx"
	}
	for _, p := range params {
		sig += ", " + p.Name + " " + p.Type
	}
	if errOnly {
		sig += ") error"
	} else {
		sig += ") (" + d.ReturnType() + ", error)"
	}
	return body, sig, needsSQL, nil
}

// dbParams derives the method parameters from the query's binds: pinned
// "name:Type" entries win; the fallback is a lowerCamel name and the uniform
// string type (FML values arrive as strings; Oracle coerces — the C decl's
// long/varchar distinction is procedural, not semantic here).
func (s *Service) dbParams(q *ir.Query, pin plan.MethodPin) ([]templates.ParamSpec, error) {
	specs := make([]templates.ParamSpec, 0, len(q.Binds))
	for i, bind := range q.Binds {
		spec := templates.ParamSpec{Name: common.CamelLowerGo(bind), Type: "string"}
		if i < len(pin.Params) {
			name, typ, hasType := strings.Cut(pin.Params[i], ":")
			if name != "" {
				spec.Name = name
			}
			if hasType && typ != "" {
				spec.Type = typ
			}
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func hasTimeParam(params []templates.ParamSpec) bool {
	for _, p := range params {
		if p.Type == "time.Time" {
			return true
		}
	}
	return false
}

// render executes one template from the service's template set. Go artifacts
// (anything starting with a package clause) are normalized through the shared
// goast.Emit gate so generated files pass the Tier-A gofmt check
// byte-for-byte; the router snippet (not Go) passes through untouched.
func (s *Service) render(id templates.ID, data any) (string, error) {
	out, err := s.tpl.Render(id, data)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(out, "package ") {
		out, err = goast.Emit("gen: "+string(id), out)
		if err != nil {
			return "", err
		}
	}
	return out, nil
}
