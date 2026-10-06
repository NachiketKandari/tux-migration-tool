package templates

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// FileProvider overlays a directory of user templates on the embedded set
// (the user-tunable template seam): <dir>/<id>.tmpl replaces that ID's
// embedded template; every other ID keeps the embedded default. A partial
// override is the normal case — missing IDs never fail.
//
// Construction validates the directory eagerly: a missing dir, a *.tmpl
// file whose stem is not a known ID, or an unparseable template fails the
// run before anything is generated, never mid-render.
type FileProvider struct {
	dir      string
	embedded *EmbeddedProvider
	cache    map[ID]*template.Template // parsed overrides, immutable after construction
	source   map[ID]string             // ID → override file path
}

// NewFileProvider builds the overlay provider for dir.
func NewFileProvider(dir string) (*FileProvider, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("templates: %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("templates: reading %s: %w", dir, err)
	}
	p := &FileProvider{
		dir:      dir,
		embedded: NewEmbeddedProvider(),
		cache:    map[ID]*template.Template{},
		source:   map[ID]string{},
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmpl") {
			continue
		}
		id := ID(strings.TrimSuffix(e.Name(), ".tmpl"))
		path := filepath.Join(dir, e.Name())
		if !registered(id) {
			return nil, fmt.Errorf("templates: %s: unknown template id %q — override files must be named <id>.tmpl for a known id (run `tuxconv templates list` for the set)", path, id)
		}
		t, perr := parseFile(path)
		if perr != nil {
			return nil, perr
		}
		p.cache[id] = t
		p.source[id] = path
	}
	return p, nil
}

// parseFile parses one override template file; the returned template is the
// root content (the file itself), mirroring the embedded loader.
func parseFile(path string) (*template.Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("templates: read %s: %w", path, err)
	}
	t, err := template.New(filepath.Base(path)).Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("templates: parse %s: %w", path, err)
	}
	return t, nil
}

// Resolve returns the run's provider: the embedded set when dir is empty,
// else the overlay. A bad override dir is an error here, at wire-up time.
func Resolve(dir string) (Provider, error) {
	if strings.TrimSpace(dir) == "" {
		return NewEmbeddedProvider(), nil
	}
	return NewFileProvider(dir)
}

// Dir returns the overlay directory.
func (p *FileProvider) Dir() string { return p.dir }

// Origin reports the override file for id; ok is false when the embedded
// template serves it.
func (p *FileProvider) Origin(id ID) (path string, ok bool) {
	path, ok = p.source[id]
	return path, ok
}

// Render implements Provider: overridden IDs execute their file, the rest
// delegate to the embedded set. Parse errors cannot occur here — every
// override parsed at construction; execution errors name the file.
func (p *FileProvider) Render(id ID, data any) (string, error) {
	if !registered(id) {
		return "", fmt.Errorf("templates: unknown template id %q", id)
	}
	t, ok := p.cache[id]
	if !ok {
		return p.embedded.Render(id, data)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("templates: execute %s (override %s): %w", id, p.source[id], err)
	}
	return sb.String(), nil
}

// Info describes one template in the effective set (for `templates list`).
type Info struct {
	ID         ID
	Origin     string // override file path, or "embedded <Version>"
	Overridden bool
	Bytes      int
}

// List reports the effective set for prov in AllIDs order: every ID with
// its origin and size. A nil provider reads as the embedded set.
func List(prov Provider) []Info {
	fp, _ := prov.(*FileProvider)
	infos := make([]Info, 0, len(AllIDs))
	for _, id := range AllIDs {
		info := Info{ID: id, Origin: "embedded " + Version}
		if data, err := templateFS.ReadFile(id.path()); err == nil {
			info.Bytes = len(data)
		}
		if fp != nil {
			if path, ok := fp.Origin(id); ok {
				info.Origin, info.Overridden = path, true
				if st, err := os.Stat(path); err == nil {
					info.Bytes = int(st.Size())
				}
			}
		}
		infos = append(infos, info)
	}
	return infos
}

// Dump writes every embedded template into dir as <id>.tmpl — the starting
// point for an override set. Existing files are kept unless force, so a
// re-dump after a tool upgrade never clobbers user edits silently.
func Dump(dir string, force bool) (written, skipped []string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("templates: create %s: %w", dir, err)
	}
	for _, id := range AllIDs {
		data, rerr := templateFS.ReadFile(id.path())
		if rerr != nil {
			return written, skipped, fmt.Errorf("templates: read embedded %s: %w", id, rerr)
		}
		path := filepath.Join(dir, string(id)+".tmpl")
		if _, serr := os.Stat(path); serr == nil && !force {
			skipped = append(skipped, path)
			continue
		}
		if werr := os.WriteFile(path, data, 0o644); werr != nil {
			return written, skipped, fmt.Errorf("templates: write %s: %w", path, werr)
		}
		written = append(written, path)
	}
	return written, skipped, nil
}

// Issue is one override-directory problem (for `templates verify`).
type Issue struct {
	Path   string
	Detail string
}

// Verify checks an override directory without generating anything: every
// *.tmpl file must carry a known ID, be non-empty, and parse. Returns the
// issues found (nil when clean); a missing directory is an error.
func Verify(dir string) ([]Issue, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("templates: %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("templates: reading %s: %w", dir, err)
	}
	var issues []Issue
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmpl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		id := ID(strings.TrimSuffix(e.Name(), ".tmpl"))
		if !registered(id) {
			issues = append(issues, Issue{Path: path, Detail: fmt.Sprintf("unknown template id %q (run `tuxconv templates list` for the set)", id)})
			continue
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			issues = append(issues, Issue{Path: path, Detail: rerr.Error()})
			continue
		}
		if strings.TrimSpace(string(data)) == "" {
			issues = append(issues, Issue{Path: path, Detail: "template file is empty — it would render nothing for " + string(id)})
			continue
		}
		if _, perr := parseFile(path); perr != nil {
			issues = append(issues, Issue{Path: path, Detail: perr.Error()})
		}
	}
	return issues, nil
}
