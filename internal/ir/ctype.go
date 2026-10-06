// Canonical C-type normalization shared by every backend namer.
//
// The extractor records HostVar.CType from the file's own declarations
// (char, varchar, int, long, ...). Spelling varies by source (VARCHAR,
// varchar2, unsigned int, ...). CanonicalCType is the one home for reducing
// those spellings to the switch values namers use. Language type maps
// (Go/C#/Python) must switch on this output, never on raw CType.
package ir

import "strings"

// CanonicalCType normalizes a C declaration type to its canonical switch
// value: lowercase, surrounding space trimmed, unsigned prefix dropped,
// varchar2 reduced to varchar, char/nchar kept distinct only as char.
// Empty input stays empty (undeclared / header vars); unknown types pass
// through lowercased so namers can fall back deterministically.
func CanonicalCType(cType string) string {
	s := strings.ToLower(strings.TrimSpace(cType))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimPrefix(s, "unsigned ")
	s = strings.TrimPrefix(s, "signed ")
	switch s {
	case "varchar2", "varchar2 ":
		return "varchar"
	case "character", "char *", "char*":
		return "char"
	case "integer":
		return "int"
	case "long int", "long long":
		return "long"
	case "short int":
		return "short"
	}
	// Strip pointer suffix for scalar switch ("char *" handled above;
	// anything else keeps its base for the namer fallback).
	if strings.HasSuffix(s, "*") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "*"))
	}
	return s
}
