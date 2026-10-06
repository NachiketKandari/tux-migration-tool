package templates

import (
	"fmt"
	"strings"
	"sync"
	"text/template"
)

// Provider is the template seam (pipeline-three-parts Part 3): the pipeline
// consumes this interface. EmbeddedProvider is the stock set; FileProvider
// (file.go) overlays a user directory on it — the user-tunable seam.
type Provider interface {
	// Render executes the template id against data and returns the result.
	Render(id ID, data any) (string, error)
}

// EmbeddedProvider renders the go:embed'ed, versioned template set distilled
// from the golden item example (PRD §4.8.6).
type EmbeddedProvider struct {
	// RWMutex, not Mutex: get is on the hottest path in the generator
	// (every DB-method render, every gentest block), and the previous
	// plain Mutex was taken even on a pure cache hit — so the parallel
	// render pools all serialized on a map lookup, and a miss held the
	// lock across template.ParseFS.
	mu    sync.RWMutex
	cache map[ID]*template.Template
}

// NewEmbeddedProvider returns the default embedded template provider.
func NewEmbeddedProvider() *EmbeddedProvider { return &EmbeddedProvider{} }

// Render implements Provider.
func (p *EmbeddedProvider) Render(id ID, data any) (string, error) {
	t, err := p.get(id)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("templates: execute %s: %w", id, err)
	}
	return sb.String(), nil
}

func (p *EmbeddedProvider) get(id ID) (*template.Template, error) {
	p.mu.RLock()
	t, ok := p.cache[id]
	p.mu.RUnlock()
	if ok {
		return t, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[ID]*template.Template)
	}
	if t, ok := p.cache[id]; ok {
		return t, nil
	}
	// Parse under the write lock: parsing twice for one ID would be
	// wasted work, and template.Template is safe to Execute concurrently
	// once built.
	t, err := load(id)
	if err != nil {
		return nil, err
	}
	p.cache[id] = t
	return t, nil
}
