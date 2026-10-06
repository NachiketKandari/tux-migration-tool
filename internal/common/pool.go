// Package common is the stdlib-only leaf: primitives shared across domain
// boundaries (bounded fan-out, text primitives, identifier casing). The
// package law is enforced by TestCommonImportsNothingInternal — it must
// never import another internal package. Anything needing audit/budget/
// scanner/etc. is domain knowledge and lives with its owning package (see
// the ownership table in AGENTS.md, AD1): "does it belong in common?" is a
// compile-checkable question, not a review debate.
package common

import "sync"

// RunIndexed runs fn(i) for i in [0, n) on a bounded worker pool and waits.
// Callers own their result slices (indexed by i), so output order always
// matches input order — workers=1 stays byte-identical to a sequential run.
func RunIndexed(n, workers int, fn func(i int)) {
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
