package pygentest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resultMarker prefixes the machine read-out a suite prints as its last act.
// A suite that prints no such line has not run to completion — the module
// raised on import, or the suite exited early — and that is a FAILURE, not a
// pass: "the suite must execute" is the plan's guard against golden-stamping,
// so a run that produced no verdict produces a non-zero verdict.
const resultMarker = "PYGENTEST-RESULT "

// Finding is one machine-readable item the suite recorded: which rule or tier
// failed, where, and what it saw. The command prints these; the exit code
// carries the verdict.
type Finding struct {
	Rule   string `json:"rule"`
	Where  string `json:"where"`
	Detail string `json:"detail"`
}

// Result is one suite run's read-out.
type Result struct {
	Suite        string
	Checks       int
	Passed       int
	Failed       int
	Skipped      int
	FailedChecks []string
	Findings     []Finding
	// Output is the suite's full stdout/stderr, kept for the operator: the
	// read-out counts, this is the evidence.
	Output string
}

// OK reports whether the run was green.
func (r Result) OK() bool { return r.Failed == 0 }

// Summary renders the one line the operator reads.
func (r Result) Summary() string {
	return fmt.Sprintf("checks=%d passed=%d failed=%d skipped=%d", r.Checks, r.Passed, r.Failed, r.Skipped)
}

// Python is the interpreter to run a suite with. Empty means "whatever
// python3 is on PATH", resolved at run time.
type Python string

// Run executes one generated suite with `python3 -S` from the suite's own
// directory, so the sibling `_pygentest_harness` / `_pygentest_sqlrules`
// modules import by name and the module under test loads from the same place
// the pipeline wrote it.
//
// -S keeps site-packages off sys.path on purpose: the plan's tooling guard
// forbids a dependency chain, and a suite that only runs where someone
// installed a package is not a gate.
//
// A RED suite is a result, not an error: Run returns the findings and the
// caller decides what to do with them. An error means the verdict itself
// could not be obtained — no interpreter, no result line, or a suite that
// contradicted itself.
func Run(python Python, suitePath string) (Result, error) {
	py := string(python)
	if py == "" {
		resolved, err := exec.LookPath("python3")
		if err != nil {
			return Result{}, errors.New("python3 is not on PATH — a suite that cannot run is a failure, not a skip; install python3 or point -python at an interpreter")
		}
		py = resolved
	}
	dir := filepath.Dir(suitePath)
	cmd := exec.Command(py, "-S", filepath.Base(suitePath))
	cmd.Dir = dir
	// PYTHONDONTWRITEBYTECODE keeps the output directory free of __pycache__
	// trees: pygentest writes into the same directory the pipeline writes, and
	// a converted tree must stay byte-comparable run to run.
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	res := Result{Output: buf.String()}
	line, ok := lastResultLine(buf.String())
	if !ok {
		if runErr != nil {
			return res, fmt.Errorf("pygentest: %s exited %v without printing a result line — the suite did not run to completion:\n%s",
				filepath.Base(suitePath), runErr, res.Output)
		}
		return res, fmt.Errorf("pygentest: %s printed no result line — the suite did not run to completion:\n%s",
			filepath.Base(suitePath), res.Output)
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		return res, fmt.Errorf("pygentest: %s printed an unreadable result line (%v):\n%s",
			filepath.Base(suitePath), err, line)
	}
	res.Output = buf.String()
	// A suite that ran and reported findings is a RESULT, not a tool error:
	// the verdict is res.Failed, and the exit code is only its echo. The one
	// combination that IS an error is a green read-out with a non-zero exit —
	// the suite disagreed with itself.
	if runErr != nil && res.OK() {
		return res, fmt.Errorf("pygentest: %s reported a clean run but exited %v:\n%s",
			filepath.Base(suitePath), runErr, res.Output)
	}
	return res, nil
}

// lastResultLine returns the payload of the final PYGENTEST-RESULT line.
func lastResultLine(out string) (string, bool) {
	var payload string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, resultMarker) {
			payload = strings.TrimSpace(strings.TrimPrefix(line, resultMarker))
		}
	}
	return payload, payload != ""
}
