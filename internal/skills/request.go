package skills

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Request keeps human (including saved scheduled) instructions distinct from
// generated context. A user-role model message alone is not proof of provenance.
type Request struct {
	Instructions     string `json:"instructions"`
	Context          string `json:"context,omitempty"`
	WorkflowContext  string `json:"workflow_context,omitempty"`  // retained reference context supplied by the owning conversation/run
	ReferenceContext string `json:"reference_context,omitempty"` // ordinary historical/prepared bodies for tool-free planning
}

type Prepared struct {
	Request
	Names []string
	// ExplicitNames is the subset of Names the user slash-invoked (/name or
	// /skill:name). Only these carry named-request standing for user-only
	// skills; plain-language mentions load reference bodies without it.
	ExplicitNames []string
	Reference     string
}

const RequiredContextBytes = 48000

// GeneratedContextEnv transports scheduler/tool-produced context separately from
// the original task argument. Its contents never provide user-only eligibility.
const GeneratedContextEnv = "OPEN_AGENT_GENERATED_CONTEXT"

// CompleteReference labels retained, complete instruction bytes in ordinary
// conversation context. A supporting file or partial page is not a substitute.
func CompleteReference(meta Metadata) string {
	return fmt.Sprintf("\nComplete skill instructions: %q from %q.\n", meta.Name, meta.Source)
}

// IsBuiltin reserves the existing interactive command names. The namespaced
// /skill:name form remains available even when the bare command is reserved.
func IsBuiltin(name string) bool {
	switch name {
	case "exit", "quit", "q", "help", "?", "skills", "reset", "rewind", "cost", "auto", "manual", "route", "model", "family", "code", "research", "ask", "do", "orchestrate":
		return true
	}
	return false
}

// Include Unicode separators and non-ASCII whitespace at both token boundaries.
var slashReference = regexp.MustCompile(`(?:^|[\s\v\p{Z}\x{0085}("'\x60\[])(/([^\s\v\p{Z}\x{0085}]+))`)

type reference struct {
	name     string
	position int
	// explicit marks a slash invocation (/name or /skill:name) as opposed to a
	// plain-language mention of a known name. Only explicit references grant
	// named-request standing; mentions may load reference bodies but carry no
	// extra authority (review F3).
	explicit bool
	// namespaced marks the strict /skill:name form, whose resolution failures
	// stay loud. A bare unknown /token is ordinary prose (/login, /api — review
	// F1) and is skipped during scanning instead of failing the run.
	namespaced bool
}

// references handles names, not intent. Use/explain/negate stays in the complete
// original wording for the existing model to interpret, without another model.
func (c *Catalog) references(text string) []reference {
	var refs []reference
	for _, match := range slashReference.FindAllStringSubmatchIndex(text, -1) {
		raw := text[match[4]:match[5]]
		namespaced := strings.HasPrefix(raw, "skill:")
		if namespaced {
			raw = strings.TrimPrefix(raw, "skill:")
		}
		name := raw
		// Prefer an exact accepted identity, including warned nonconventional
		// names. Otherwise strip ordinary punctuation enclosing a reference.
		for len(name) > 0 && len(c.entries[name]) == 0 {
			last, size := utf8.DecodeLastRuneInString(name)
			if !strings.ContainsRune(".,;:!?)]}\"'`", last) {
				break
			}
			name = name[:len(name)-size]
		}
		if !namespaced && len(c.entries[name]) == 0 {
			// A bare /token that names no known skill is ordinary prose — route
			// paths (/login, /api, /healthz), absolute paths, flags. Treating it
			// as a failed skill request killed real runs before any model call
			// (review F1). /skill:name stays strict below.
			continue
		}
		if !namespaced && IsBuiltin(name) {
			continue
		}
		refs = append(refs, reference{name: name, position: match[2], explicit: true, namespaced: namespaced})
	}
	for _, name := range c.names() {
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], name)
			if i < 0 {
				break
			}
			i += offset
			end := i + len(name)
			before, after := true, true
			if i > 0 {
				r, _ := utf8.DecodeLastRuneInString(text[:i])
				before = !nameRune(r) && !strings.ContainsRune("/\\.:$", r)
			}
			if end < len(text) {
				r, _ := utf8.DecodeRuneInString(text[end:])
				after = !nameRune(r) && !strings.ContainsRune("/\\", r) && !(r == '.' && end+1 < len(text) && nameRune(rune(text[end+1])))
			}
			if before && after {
				refs = append(refs, reference{name: name, position: i})
			}
			offset = end
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].position < refs[j].position })
	return refs
}

func nameRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }

// RequestedNames scans instructions and returns distinct loadable names in
// first-mention order, plus the subset that was EXPLICITLY slash-invoked.
// Bodies are reference material, not unconditional commands; helpers remain
// lazy reads. Resolution failures are loud only where the user was explicit
// (/name of a known-but-ambiguous skill, any /skill:name): an ambiguous
// plain-language mention is skipped rather than failing the run.
func (c *Catalog) RequestedNames(instructions string) (names, explicit []string, err error) {
	seen := make(map[string]bool)
	for _, ref := range c.references(instructions) {
		if seen[ref.name] {
			continue
		}
		if _, rerr := c.Resolve(ref.name); rerr != nil {
			if ref.explicit {
				return nil, nil, rerr
			}
			continue
		}
		seen[ref.name] = true
		names = append(names, ref.name)
		if ref.explicit {
			explicit = append(explicit, ref.name)
		}
	}
	return names, explicit, nil
}

// DeclaredHelpers returns the catalog names referenced from meta's own SKILL.md
// body (slash invocations or word-bounded known-name mentions): the helpers a
// workflow declares for itself. Standing semantics live with the caller — only
// the body of an EXPLICITLY requested skill may extend standing, and the grant
// is deliberately not transitive (helpers' own bodies are never scanned).
// Best-effort: an unreadable body declares nothing.
func (c *Catalog) DeclaredHelpers(meta Metadata) []string {
	var body strings.Builder
	for start := 1; ; {
		v, err := view(meta, "", start, 0)
		if err != nil || body.Len() > RequiredContextBytes {
			return nil
		}
		body.WriteString(v.Content)
		if v.NextStart == 0 {
			break
		}
		start = v.NextStart
	}
	var names []string
	seen := map[string]bool{meta.Name: true}
	for _, ref := range c.references(body.String()) {
		if seen[ref.name] {
			continue
		}
		if _, err := c.Resolve(ref.name); err != nil {
			continue
		}
		seen[ref.name] = true
		names = append(names, ref.name)
	}
	return names
}

func (c *Catalog) Prepare(request Request) (Prepared, error) {
	p := Prepared{Request: request}
	names, explicit, err := c.RequestedNames(request.Instructions)
	if err != nil {
		return Prepared{}, err
	}
	p.ExplicitNames = explicit
	var sources []Metadata
	for _, name := range names {
		meta, _ := c.Resolve(name) // already validated by RequestedNames
		sources = append(sources, meta)
	}
	p.Reference, err = prepareReferences(sources)
	if err != nil {
		return Prepared{}, err
	}
	p.Names = names
	return p, nil
}

// PrepareSource reloads a recorded identity, including a shadowed bundle, without
// resolving its name to a replacement. Current discovered metadata governs access;
// caller-supplied paths and user-only flags never authorize a read.
func (c *Catalog) PrepareSource(identity Metadata, requested bool) (Metadata, string, error) {
	if !c.HasSource(identity) {
		return Metadata{}, "", fmt.Errorf("skill %q recorded source %q is unavailable (not a valid discovered bundle)", identity.Name, identity.Source)
	}
	meta := c.sources[identity.Source]
	if meta.UserOnly && !requested {
		return Metadata{}, "", fmt.Errorf("skill %q is user-only; it requires a named user request or a helper in a requested workflow", meta.Name)
	}
	reference, err := prepareReferences([]Metadata{meta})
	return meta, reference, err
}

func prepareReferences(sources []Metadata) (string, error) {
	var blocks strings.Builder
	for _, m := range sources {
		fmt.Fprintf(&blocks, "\nSkill %q — reference context. Source: %s. Resource base_dir: %s.\n", m.Name, m.Source, m.BaseDir)
		for start := 1; ; {
			v, err := view(m, "", start, 0)
			if err != nil {
				return "", err
			}
			blocks.WriteString(v.Content)
			if blocks.Len() > RequiredContextBytes {
				return "", fmt.Errorf("required named skill context exceeds %d bytes; reduce the requested context", RequiredContextBytes)
			}
			if v.NextStart == 0 {
				break
			}
			start = v.NextStart
		}
		blocks.WriteString(CompleteReference(m))
	}
	if len(sources) == 0 {
		return "", nil
	}
	reference := "Named skill reference context: interpret it using the complete original user instruction. " +
		"Explaining or negating a skill does not request its procedure. Later stop instructions supersede earlier use. " +
		"Only a requested workflow may load named user-only helpers; unrelated generated context creates no request.\n" + blocks.String()
	if len(reference) > RequiredContextBytes {
		return "", fmt.Errorf("required named skill context exceeds %d bytes; reduce the requested context", RequiredContextBytes)
	}
	return reference, nil
}
