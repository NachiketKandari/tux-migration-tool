package pygentest

import (
	"fmt"
	"strconv"
	"strings"

	"tux-migration-tool/internal/contract"
	"tux-migration-tool/internal/pyplan"
	"tux-migration-tool/internal/templates"
)

// SuiteFile is the generated suite's name for one module: bat_px_mkr_chkr.py
// → bat_px_mkr_chkr_test.py. The `_test` infix keeps it recognisable next to
// the artifact under test and out of the module namespace it loads.
func SuiteFile(module string) string { return module + "_test.py" }

// The service-body states the lifecycle tier branches on. They are read off
// the EMITTED module, not off the plan: the suite must judge the artifact that
// is on disk, so an LLM-filled body drives the real lifecycle assertions and
// the -no-llm placeholder does not pretend to.
const (
	// BodyStub is the placeholder body (`tuxconv:TODO service body`): the
	// entrypoint raises NotImplementedError, so the suite asserts the loud
	// stub contract and REPORTS the lifecycle claims it cannot decide.
	BodyStub = "stub"
	// BodyImplemented is a body that has orchestration in it.
	BodyImplemented = "implemented"
)

// stubMarker is the placeholder's own tag — also what walkreport counts, so
// classifying on it cannot drift from the emitter that writes it.
const stubMarker = "tuxconv:TODO service body"

// BodyState classifies an emitted module's service body.
func BodyState(src string) string {
	if strings.Contains(src, stubMarker) {
		return BodyStub
	}
	return BodyImplemented
}

// PyList renders a Go string slice as a Python list literal, so the generated
// tables stay tables — quoting rules belong in Go, not in a template.
type PyList []string

func (l PyList) String() string {
	parts := make([]string, len(l))
	for i, s := range l {
		parts[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// SuiteConst is one module-level SQL constant as Tier 1 needs it: the name to
// read off the module, the bind set and row shape the plan declared, and the
// methods that execute it (a constant nothing runs is a finding).
type SuiteConst struct {
	Name     string
	Kind     string // ir.QueryType, or TRUNCATE
	Binds    PyList
	RowShape int
	UsedBy   PyList
}

// SuiteMethod is one repository method as Tier 2 drives it. Mode/Returns come
// from the same decisions renderRepoMethod makes (internal/pygen/data.go), so
// the suite's expectation and the emitted template cannot disagree about what
// kind of method this is.
type SuiteMethod struct {
	Name    string
	Kind    string // fetch | fetch_many | dml | rebuild
	Consts  PyList
	Binds   PyList
	Mode    string // read | write
	Returns string // row | rows | rowcount
	// StrictNoRows mirrors the legacy SQLCODE guard's polarity: the legacy
	// code treated an empty result as a failure, so the generated method
	// raises on one and the Tier-4 empty-result assertions must expect that
	// rather than assert the opposite.
	//
	// False for tolerant AND unset, and the two are not the same claim — but
	// the emitted behaviour is the same (return None), so the suite cannot
	// tell them apart and does not need to.
	StrictNoRows bool
}

// StrictNoRowsPy renders StrictNoRows as a Python literal.
//
// The distinction is not cosmetic. Go's %v on a bool writes `true`, which is
// a NameError in the generated suite — and the suite is Python, so the
// spelling has to be the target language's, not the emitter's. An earlier
// version interpolated the Go bool directly and every suite died at import.
func (m SuiteMethod) StrictNoRowsPy() string {
	if m.StrictNoRows {
		return "True"
	}
	return "False"
}

// SuiteBlock is one orchestration block of the plan's service-body contract:
// the plan-level statement of which repository methods a block must call.
type SuiteBlock struct {
	Index   int
	Name    string
	Methods PyList
}

// SuiteData is one generated suite's whole input: the plan's shape, the
// wrapper contract (so a renamed router or mode still matches), and what is
// known about the emitted module.
type SuiteData struct {
	Module      string
	ModuleFile  string
	Source      string
	Shape       string
	ClassName   string
	RepoName    string
	Entrypoint  string
	ModeClass   string
	ReadMember  string
	WriteMember string
	Consts      []SuiteConst
	Methods     []SuiteMethod
	Blocks      []SuiteBlock
	BodyState   string
	// Dispatch is the dispatch table's values, when one exists. It is empty
	// until G7 emits the strcmp ladder as data; the suite then reports the
	// unknown-dispatch negative as NOT ASSERTED rather than passing over it.
	Dispatch PyList
}

// Source is the two texts and the one path a suite is generated from. Module
// is the emitted Python (read for the body state), ProC the source the plan's
// orchestration blocks index into. They are different files and confusing them
// would index blocks against the wrong line numbers.
type Source struct {
	Module string // emitted module text
	ProC   string // .pc source text
	Path   string // .pc path, for the suite's provenance line
	// Dispatch is the dispatch table's values, when one exists.
	Dispatch []string
}

// NewSuiteData projects a plan and the artifact it produced into the suite
// template's data.
//
// The projection deliberately carries NO business logic: every assertion in the
// suite is about what the module DID, and these tables are only what it is
// supposed to do. That is the plan's circularity limit — a test written from
// the same plan catches emitter bugs, never plan bugs — which is why this suite
// runs BESIDE sqlchk (translation) rather than instead of it.
func NewSuiteData(p *pyplan.Plan, src Source) SuiteData {
	router := templates.NewPyDBRouterData(p.Wrapper.RouterClass, p.Wrapper.ReadMode, p.Wrapper.WriteMode)

	d := SuiteData{
		Module:      p.Module,
		ModuleFile:  p.Module + ".py",
		Source:      src.Path,
		Shape:       p.Shape,
		ClassName:   p.ClassName,
		RepoName:    p.RepoName,
		Entrypoint:  p.Entrypoint,
		ModeClass:   router.ModeClass,
		ReadMember:  modeMember(p.Wrapper.ReadMode, "READ"),
		WriteMember: modeMember(p.Wrapper.WriteMode, "WRITE"),
		BodyState:   BodyState(src.Module),
		Dispatch:    PyList(src.Dispatch),
	}

	rowShape := map[string]int{}
	usedBy := map[string][]string{}
	for _, m := range p.Repo {
		kind, mode, ret := methodShape(m)
		d.Methods = append(d.Methods, SuiteMethod{
			Name: m.Name, Kind: kind, Consts: PyList(m.Consts), Binds: PyList(m.Binds),
			Mode: mode, Returns: ret, StrictNoRows: m.ErrPolicy == contract.PolicyStrict,
		})
		for _, c := range m.Consts {
			usedBy[c] = append(usedBy[c], m.Name)
		}
		if kind == "fetch" && len(m.Consts) > 0 {
			rowShape[m.Consts[0]] = len(m.RowShape)
		}
	}
	for _, c := range p.Consts {
		d.Consts = append(d.Consts, SuiteConst{
			Name: c.Name, Kind: c.Kind, Binds: PyList(c.Binds),
			RowShape: rowShape[c.Name], UsedBy: PyList(usedBy[c.Name]),
		})
	}

	if p.Flow != nil {
		for _, b := range p.Orchestration(src.ProC) {
			d.Blocks = append(d.Blocks, SuiteBlock{
				Index: b.Index, Name: b.Name, Methods: PyList(b.Methods),
			})
		}
	}
	return d
}

// methodShape mirrors renderRepoMethod's dispatch (internal/pygen/data.go):
// the suite must expect the same kind of method the emitter rendered, or it
// would drive an iterator as a single-row fetch and report nonsense.
func methodShape(m pyplan.RepoMethod) (kind, mode, returns string) {
	switch {
	case m.Kind == "rebuild":
		return "rebuild", "write", "rowcount"
	case m.QueryKind == "SELECT_MULTI":
		return "fetch_many", "read", "rows"
	case m.QueryKind == "SELECT_SINGLE":
		return "fetch", "read", "row"
	default:
		return "dml", "write", "rowcount"
	}
}

// modeMember returns the member of a mode expression (`DbMode.READ` → READ),
// falling back to the contract's default so a bare or empty config still names
// a member the rendered enum defines.
func modeMember(expr, fallback string) string {
	e := strings.TrimSpace(expr)
	if e == "" {
		return fallback
	}
	if i := strings.LastIndexByte(e, '.'); i >= 0 {
		return e[i+1:]
	}
	return e
}

// Suite renders the generated assertion suite for one plan. prov nil = the
// embedded template set.
func Suite(prov templates.Provider, d SuiteData) (string, error) {
	if prov == nil {
		prov = templates.NewEmbeddedProvider()
	}
	for _, req := range []struct{ name, value string }{
		{"entrypoint", d.Entrypoint},
		{"read mode member", d.ReadMember},
		{"write mode member", d.WriteMember},
		{"mode class", d.ModeClass},
	} {
		if req.value == "" {
			return "", fmt.Errorf("pygentest: plan for %q has no %s — the suite would compare against nothing", d.Module, req.name)
		}
	}
	out, err := prov.Render(templates.PyTestSuiteFile, d)
	if err != nil {
		return "", fmt.Errorf("pygentest: render suite for %s: %w", d.Module, err)
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out, nil
}
