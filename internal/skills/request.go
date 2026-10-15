package skills

import (
	"fmt"
	"os"
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
	Names     []string
	Reference string
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
var conventionalReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type reference struct {
	name     string
	position int
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
			if strings.ContainsAny(name, "/\\.") || !conventionalReference.MatchString(name) {
				continue
			}
			// A single-component absolute path has the same spelling as a bare
			// command. Existing paths remain paths; /skill:name forces resolution.
			// Known skill identities above retain their explicit invocation meaning.
			if _, err := os.Lstat("/" + name); err == nil {
				continue
			}
		}
		if !namespaced && IsBuiltin(name) {
			continue
		}
		refs = append(refs, reference{name, match[2]})
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
				refs = append(refs, reference{name, i})
			}
			offset = end
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].position < refs[j].position })
	return refs
}

func nameRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }

// Prepare loads distinct named references in first-mention order. Bodies are
// reference material, not unconditional commands. Helpers remain lazy reads.
func (c *Catalog) RequestedNames(instructions string) ([]string, error) {
	var names []string
	seen := make(map[string]bool)
	for _, ref := range c.references(instructions) {
		if seen[ref.name] {
			continue
		}
		seen[ref.name] = true
		if _, err := c.Resolve(ref.name); err != nil {
			return nil, err
		}
		names = append(names, ref.name)
	}
	return names, nil
}

func (c *Catalog) Prepare(request Request) (Prepared, error) {
	p := Prepared{Request: request}
	names, err := c.RequestedNames(request.Instructions)
	if err != nil {
		return Prepared{}, err
	}
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
