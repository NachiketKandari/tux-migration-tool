package common

import (
	"regexp"
	"sync"
)

// rxCache memoizes compiled regexps for dynamically built patterns
// (QuoteMeta(literal) + fixed framing). MustCompile cost is paid once per
// distinct pattern string; returned pointers are immutable and safe for
// concurrent use. This preserves exact match semantics — it only avoids
// recompiling the same pattern on every call in hot loops.
var rxCache sync.Map // map[string]*regexp.Regexp

// CachedRegexp returns the compiled pattern, compiling once per distinct
// string. The pattern must already be fully assembled by the caller.
func CachedRegexp(pattern string) *regexp.Regexp {
	if v, ok := rxCache.Load(pattern); ok {
		if re, ok := v.(*regexp.Regexp); ok && re != nil {
			return re
		}
	}
	re := regexp.MustCompile(pattern)
	rxCache.Store(pattern, re)
	return re
}
