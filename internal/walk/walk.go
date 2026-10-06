// Package walk is the endpoint-wide provenance index for the deterministic
// controller synthesizer.
//
// The shaping renderer (internal/gen) is handed one read at a time and asked
// to satisfy the endpoint's whole response from it. That question has no
// answer in general: a response field is a write to one specific host
// variable, and that host is produced by exactly one read — or by none. Asking
// read B about a field that read A produces can only ever return "no match",
// which is why the same response fields were reported unmapped once per read
// in the endpoint instead of once.
//
// Scope answers the question the renderer actually needs: given the host a
// response write read from, which read produces it?
//
// It is deliberately a pure index over facts that already exist. It resolves
// nothing itself and renders nothing; it says which read owns a host, or that
// none does. Deciding what to emit from that — a row reference, a request
// copy, or a loud gap — stays with the renderer.
package walk

import (
	"strings"
)

// Read is one row-returning store call in the endpoint's walk: the query it
// came from, the Go capture it binds to, and the row shape it yields.
//
// Hosts and Fields are parallel — Hosts[i] is the legacy host variable bound
// to the i-th select-list column and Fields[i] is the Go row field the models
// package emits for it. Either may be shorter than the other when a row shape
// is only partly known; index both defensively.
type Read struct {
	// QueryID is the IR query id ("q4", "cur_get_tblc_dtls"); "" when the
	// call has no IR record.
	QueryID string
	// Capture is the Go variable the read's result binds to
	// ("getGetTblcDtlsRows").
	Capture string
	// RowType is the models row struct name ("GetTblcDtls").
	RowType string
	// Hosts are the legacy host variables of the row shape, in select-list
	// order. Normalized on the way in.
	Hosts []string
	// Fields are the Go row field names, parallel to Hosts.
	Fields []string
}

// Owner records which read produces one host, and the row field it lands in.
type Owner struct {
	// Read is the index into Scope.Reads.
	Read int
	// Field is the Go row field name.
	Field string
	// Host is the normalized host variable, as indexed.
	Host string
}

// Scope is the endpoint-wide index of which read produces which host.
//
// Reads are in the endpoint's deterministic call order, and OwnerFor returns
// the FIRST read that produces a host. First-wins is the same rule the
// renderer already used for response targets; stating it here makes it a
// property of the index rather than an accident of map iteration.
type Scope struct {
	// Reads are the endpoint's row-returning reads, in walk order.
	Reads []Read
	// owners maps a normalized host to the read that produces it.
	owners map[string]Owner
}

// Index builds the endpoint-wide scope.
//
// reads must be in the endpoint's deterministic call order — that order is
// what makes first-wins well defined. A read with no usable row shape still
// occupies its slot in Reads (so indices stay stable) but contributes no
// owners.
func Index(reads []Read) *Scope {
	s := &Scope{Reads: make([]Read, len(reads))}
	copy(s.Reads, reads)
	s.owners = make(map[string]Owner)
	for i, r := range s.Reads {
		n := len(r.Hosts)
		if len(r.Fields) < n {
			n = len(r.Fields)
		}
		for j := 0; j < n; j++ {
			host := NormalizeHost(r.Hosts[j])
			if host == "" || r.Fields[j] == "" {
				continue
			}
			if _, seen := s.owners[host]; seen {
				continue // first read wins — deterministic
			}
			s.owners[host] = Owner{Read: i, Field: r.Fields[j], Host: host}
		}
	}
	return s
}

// OwnerFor returns the read that produces a host variable, and whether any
// read does. The host is normalized first, so callers may pass the raw
// identifier, a dotted member, or an ".arr"/"[0]" form.
func (s *Scope) OwnerFor(host string) (Owner, bool) {
	if s == nil {
		return Owner{}, false
	}
	o, ok := s.owners[NormalizeHost(host)]
	return o, ok
}

// ReadFor returns the index of the read whose capture is name, and whether it
// is in scope. Captures are unique within an endpoint, so this is a lookup
// and not a search.
func (s *Scope) ReadFor(capture string) (int, bool) {
	if s == nil {
		return 0, false
	}
	for i := range s.Reads {
		if s.Reads[i].Capture == capture {
			return i, true
		}
	}
	return 0, false
}

// Len is the number of reads in scope.
func (s *Scope) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Reads)
}

// NormalizeHost maps a legacy host reference to the one key every bridge in
// the codebase agrees on: drop a trailing comment, drop a trailing ".arr",
// take the last dotted member, drop "[i]", lowercase, and strip a leading
// "sql_".
//
// This MUST agree with gen.normHost, which keys the renderer's maps. The two
// are pinned against each other by TestNormalizeHostAgreesWithGenNormHost in
// internal/gen — the package that can see both — so a change to either one
// fails loudly instead of silently splitting the index from the renderer.
func NormalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if j := strings.IndexAny(s, " \t"); j > 0 {
		s = s[:j]
	}
	if j := strings.LastIndex(s, "."); j >= 0 {
		s = s[j+1:]
	}
	s = strings.TrimSuffix(s, ".arr")
	s = strings.TrimSuffix(s, "[0]")
	if j := strings.Index(s, "["); j >= 0 {
		s = s[:j]
	}
	s = strings.TrimSuffix(s, ".arr")
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sql_")
	return strings.Trim(s, "_")
}
