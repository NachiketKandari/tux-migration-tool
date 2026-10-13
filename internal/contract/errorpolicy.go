package contract

import "strings"

// ErrorPolicy is the failure contract a query's post-SQL status guard declares.
//
// The legacy Pro*C idiom is `if (SQLCODE != 0)`, and the trap it sets is that
// SQLCODE is zero on success and non-zero on *anything else* — including
// NO_DATA_FOUND (Oracle status 100), which is not a failure at all, merely an
// empty result. So the naive spelling silently makes "query succeeded, found
// nothing" into an error, and the corrected spelling has to say so out loud:
//
//	if (SQLCODE != 0)                                  → strict
//	if (SQLCODE != 0 && SQLCODE != NO_DATA_FOUND)      → tolerant
//
// The two differ in behaviour, not in style, and they are not
// interchangeable: the corpus contains sites of each following the same kind
// of query. This type carries the difference so no emitter has to re-derive it
// from the SQL verb — an inference `internal/testgen/methods.go:153-164`
// records as having already produced one wrong answer and one wrong test.
//
// Language-neutral by construction: it names a contract, not a Go `error`, a
// C# exception, or a Python exception. Each emitter projects it onto its own
// error idiom.
type ErrorPolicy string

const (
	// PolicyStrict — `if (SQLCODE != 0)`, and the bare `if (SQLCODE)`
	// truthiness form that C makes equivalent to it. Any nonzero status is a
	// failure, NO_DATA_FOUND included: an empty result here is an error the
	// caller must see.
	PolicyStrict ErrorPolicy = "strict"

	// PolicyTolerantNoRows — `if (SQLCODE != 0 && SQLCODE != NO_DATA_FOUND)`.
	// An empty result is a success and must not surface as an error. This is
	// the spelling that says "I know 100 is not a failure".
	PolicyTolerantNoRows ErrorPolicy = "tolerant_no_rows"

	// PolicyUnclassified — a status-referencing branch the classifier could
	// not read with confidence, most often a predicate that OR-s SQLCODE with
	// an unrelated business test. Never silently resolved to strict: it is
	// carried so the census can count it (see
	// walkreport.ReasonErrorPolicyUnclassified). A guess here would be
	// emitted code, which is worse than a failing test.
	PolicyUnclassified ErrorPolicy = "unclassified"
)

// IsSet reports whether a policy was actually read from a guard. An unset
// policy means the legacy source stated nothing about this query's failure
// contract, which is distinct from PolicyStrict: the source did not say "an
// empty result is an error", it said nothing. Emitters keep their existing
// per-type behaviour for unset, so adopting this policy never silently
// rewrites a query whose guard was not read.
func (p ErrorPolicy) IsSet() bool { return p != "" }

// ToleratesNoRows reports whether an empty result is a success under the
// policy. Unset is deliberately false: absent a guard, the emitter's own
// default applies, and this predicate must not override it.
func (p ErrorPolicy) ToleratesNoRows() bool { return p == PolicyTolerantNoRows }

// Strict reports whether the policy pins an empty result to failure. Unset is
// false for the same reason ToleratesNoRows is false — unset defers to the
// emitter's own default rather than asserting either polarity.
func (p ErrorPolicy) Strict() bool { return p == PolicyStrict }

// BenignStatusCodes are the nonzero SQLCODE values that mean "the statement
// ran and there was simply nothing to return", not "the statement failed".
//
// NO_DATA_FOUND (100) is the one the corpus names. It is listed rather than
// special-cased so that adding another is a one-line change with no new rule:
// the classifier's job is to notice that a guard excludes a status the
// surrounding `!= 0` would have fired on.
//
// Anything not in this table that is non-zero is a failure. That is the
// conservative direction — an unrecognised code keeps the legacy error path —
// and it is why an unknown code must not be added here speculatively.
var BenignStatusCodes = map[string]bool{
	"NO_DATA_FOUND": true,
}

// BenignStatus reports whether name (case-insensitively) is a status code
// that means empty-result-rather-than-failure.
func BenignStatus(name string) bool { return BenignStatusCodes[upperASCII(name)] }

// StatusRegisters are the spellings the corpus uses for the post-statement
// status register. SQLCODE is the global Pro*C one; the SQLERR forms appear
// in embedded-SQL idioms that name the sqlca field directly.
var StatusRegisters = []string{
	"SQLCODE",
	"SQLERR",
	"SQLCA.SQLERR",
	"SQLCA.SQLCODE",
}

// IsStatusRegister reports whether name is one of the status registers this
// classifier reads. Comparison is case-folded and ignores an optional "s_"
// prefix, because Pro*C sources in this corpus spell it both ways.
func IsStatusRegister(name string) bool {
	n := upperASCII(name)
	n = trimStatusPrefix(n)
	for _, r := range StatusRegisters {
		if n == r {
			return true
		}
	}
	return false
}

// trimStatusPrefix drops the conventional S_ prefix the status register
// carries in some sources, so "s_sqlcode" and "SQLCODE" are one identifier.
// The argument is expected already case-folded.
func trimStatusPrefix(n string) string {
	if strings.HasPrefix(n, "S_") {
		return n[2:]
	}
	return n
}

// upperASCII uppercases ASCII letters only, leaving non-ASCII bytes alone —
// the same fold the scanner's token compare uses, so a status name matches
// here exactly when it matched there.
func upperASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
