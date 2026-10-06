package gen

import (
	"strings"

	"tux-migration-tool/internal/ir"
)

// GoTypeFor is the canonical Go scalar for a host variable: the one home
// for the C→Go projection that used to live as ir.HostVar.GoHint
// (uniform-ir plan §3.2). It switches on the canonical C type, falling back
// to the stored hint only when the declaration is absent but a bare hint
// survived extraction.
//
// Callers must prefer this over reading HostVar.GoHint directly; the IR
// field stays populated as a compatibility shim until Phase 5 removes it.
func GoTypeFor(hv ir.HostVar) string {
	canon := ir.CanonicalCType(hv.CType)
	if canon != "" {
		switch {
		case strings.Contains(canon, "char"), strings.Contains(canon, "varchar"):
			return "string"
		case strings.Contains(canon, "double"), strings.Contains(canon, "float"):
			return "float64"
		case strings.Contains(canon, "long"):
			return "int64"
		case strings.Contains(canon, "short"):
			return "int"
		case strings.Contains(canon, "int"):
			return "int"
		case strings.Contains(canon, "time"):
			return "string"
		}
	}
	if hv.GoHint != "" {
		return hv.GoHint
	}
	return ""
}

// GoTypeForCType is the declaration-only variant used when no HostVar is at
// hand (synthetic derivations, tests).
func GoTypeForCType(cType string) string {
	return GoTypeFor(ir.HostVar{CType: cType})
}
