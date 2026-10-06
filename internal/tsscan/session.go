package tsscan

import (
	"context"
	_ "embed"
	"sync"

	"github.com/zema1/wasitter"
)

// wasitterCWasm is the upstream tree-sitter runtime plus the tree-sitter-c
// v0.24.2 grammar, compiled to WASM (wasitter v0.1.0 release asset, sha256
// 5044f382aa9d3b8c200947272c62e9eb5d0678e56089171c54a2e821005286b2). It runs
// under the pure-Go wazero runtime, so the toolchain needs no cgo and no C
// compiler on any platform.
//
//go:embed grammars/wasitter-c.wasm
var wasitterCWasm []byte

// scanSession owns one wasitter runtime + parser pair. Sessions are pooled
// and reused across files: a wasitter runtime serializes guest execution,
// so a pooled session must never be shared concurrently — each ScanBytes
// call checks one out exclusively and returns it when done. Reuse is safe
// because Parser.Parse carries no retained parse state (the prior tree is
// closed before return); it only avoids re-instantiating the WASM module
// per file, which dominates analyze time.
type scanSession struct {
	parser  *wasitter.Parser
	runtime *wasitter.Runtime
}

// sessionPool holds idle sessions for reuse across ScanBytes calls.
var sessionPool sync.Pool

func newScanSession() (*scanSession, error) {
	parser, runtime, err := wasitter.NewParserFromWASM(context.Background(), wasitterCWasm)
	if err != nil {
		return nil, err
	}
	return &scanSession{parser: parser, runtime: runtime}, nil
}

// getScanSession returns an idle pooled session or constructs a fresh one.
// The caller owns it exclusively until putScanSession (success) or
// closeSession (parse-level failure that may have poisoned the runtime).
func getScanSession() (*scanSession, error) {
	if v := sessionPool.Get(); v != nil {
		if s, ok := v.(*scanSession); ok && s != nil && s.parser != nil {
			return s, nil
		}
	}
	return newScanSession()
}

// putScanSession returns a healthy session to the pool for reuse.
func putScanSession(s *scanSession) {
	if s == nil || s.parser == nil || s.runtime == nil {
		return
	}
	sessionPool.Put(s)
}

// closeSession discards a session that may be unusable (parse ABI error or
// cancellation can leave the runtime closed per wasitter docs). It is not
// returned to the pool.
func closeSession(s *scanSession) {
	if s == nil {
		return
	}
	s.close(nil)
}

// close releases the tree (if any), then the parser, then the runtime.
func (s *scanSession) close(tree *wasitter.Tree) {
	if tree != nil {
		tree.Close()
	}
	s.parser.Close()
	s.runtime.Close()
}
