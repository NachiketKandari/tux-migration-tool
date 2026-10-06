package common

import "strings"

// Leading returns the leading whitespace run (spaces and tabs) of line —
// the indentation prefix. The one home for the four byte-identical copies
// that lived in pychk, pyplan, pygen, and budget (A3.1).
func Leading(line string) string {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return line[:i]
		}
	}
	return line
}

// UniqueStable deduplicates strings preserving first-appearance order.
// The one home for the four inline implementations (batchflow uniqueNames,
// pyplan bindOrder, convert requiredCalls, gen contractFields — A3.1).
func UniqueStable(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// NormalizeWS collapses all whitespace runs to single spaces and trims the
// edges — the SQL/condition text normalization the parse stack and the cmd
// helpers re-derive from strings.Fields (A3.1).
func NormalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
