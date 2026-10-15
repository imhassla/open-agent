package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

type skillSource struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	BaseDir string `json:"base_dir"`
}

// This is ordinary reference context stored in transcript messages. Requested
// records provenance, not whether a procedure is active; the original wording
// and subsequent instructions govern use, explanation and stopping.
type skillMemory struct {
	Meaning      string        `json:"meaning"`
	Sources      []skillSource `json:"sources"`
	Instructions []string      `json:"original_user_instructions_in_order"`
	Requested    bool          `json:"named_user_context"`
}

const skillMemoryMeaning = "Previously inspected skill reference context, not mandatory activation. Interpret original user instructions in order, including explanation, negation and later stops. Reload with skill_view when needed; historical bytes are not automatically refreshed."

func decodeSkillMemory(text string) (skillMemory, bool) {
	var memory skillMemory
	err := json.Unmarshal([]byte(text), &memory)
	return memory, err == nil && len(memory.Sources) > 0
}

// WorkflowContext returns the latest valid reminder from the owner's transcript.
// It does not infer requests from arbitrary generated prose.
func WorkflowContext(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Name == "skill_reminder" {
			if _, ok := decodeSkillMemory(history[i].Content); ok {
				return history[i].Content
			}
		}
	}
	return ""
}

// WithSkillReminder replaces historical reminders with the current one while
// retaining all other messages (including complete tool exchanges) in order.
func WithSkillReminder(history []llm.Message, reminder string) []llm.Message {
	result := make([]llm.Message, 0, len(history)+1)
	for _, message := range history {
		if message.Name != "skill_reminder" {
			result = append(result, message)
		}
	}
	if reminder != "" {
		result = append(result, llm.Message{Role: "user", Name: "skill_reminder", Content: reminder})
	}
	return result
}

// MergeSkillSources adds references loaded by owned workers. Generated artifact
// text is never parsed as provenance, and a child cannot upgrade named eligibility.
func MergeSkillSources(base string, contexts ...string) string {
	memory, _ := decodeSkillMemory(base)
	for _, context := range contexts {
		child, ok := decodeSkillMemory(context)
		if !ok {
			continue
		}
		for _, source := range child.Sources {
			memory.addSource(skills.Metadata{Name: source.Name, Source: source.Source, BaseDir: source.BaseDir})
		}
		if len(memory.Instructions) == 0 {
			memory.Instructions = append([]string(nil), child.Instructions...)
		}
	}
	if len(memory.Sources) == 0 {
		return ""
	}
	data, _ := json.Marshal(memory)
	return string(data)
}

func (a *Agent) RememberSkillSources(context string) {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	base, _ := json.Marshal(a.skillMemory)
	if memory, ok := decodeSkillMemory(MergeSkillSources(string(base), context)); ok {
		a.skillMemory = memory
	}
}

func (a *Agent) restoreSkillMemory(prior []llm.Message) {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	a.skillMemory = skillMemory{}
	a.skillTurn = nil
	a.skillExecution = ""
	for i := len(prior) - 1; i >= 0; i-- {
		if prior[i].Name == "skill_reminder" {
			if memory, ok := decodeSkillMemory(prior[i].Content); ok {
				a.skillMemory = memory
				return
			}
		}
	}
}

func (m *skillMemory) addSource(meta skills.Metadata) {
	for _, source := range m.Sources {
		if source.Name == meta.Name && source.Source == meta.Source {
			return
		}
	}
	m.Sources = append(m.Sources, skillSource{Name: meta.Name, Source: meta.Source, BaseDir: meta.BaseDir})
	m.Meaning = skillMemoryMeaning
}

func (m *skillMemory) rememberRequest(catalog *skills.Catalog, names []string, instruction string) error {
	for _, name := range names {
		meta, err := catalog.Resolve(name)
		if err != nil {
			return err
		}
		m.addSource(meta)
	}
	m.Requested = m.Requested || len(names) > 0
	if !m.rememberInstruction(instruction) {
		return fmt.Errorf("required original skill instruction context exceeds %d bytes", skills.RequiredContextBytes)
	}
	return nil
}

func (m *skillMemory) rememberInstruction(instruction string) bool {
	if len(m.Sources) > 0 && (len(m.Instructions) == 0 || m.Instructions[len(m.Instructions)-1] != instruction) {
		prior := m.Instructions
		m.Instructions = append(m.Instructions, instruction)
		data, _ := json.Marshal(m)
		if len(data) > skills.RequiredContextBytes {
			m.Instructions = prior
			return false
		}
	}
	return true
}

// RememberSkillInstruction retains intent from a failed planning turn without
// resolving references or authorizing any new skill.
func RememberSkillInstruction(reminder, instruction string) (string, error) {
	memory, ok := decodeSkillMemory(reminder)
	if !ok {
		return "", nil
	}
	if !memory.rememberInstruction(instruction) {
		return reminder, fmt.Errorf("required original skill instruction context exceeds %d bytes", skills.RequiredContextBytes)
	}
	data, _ := json.Marshal(memory)
	return string(data), nil
}

func (a *Agent) skillReminder() string {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	if len(a.skillMemory.Sources) == 0 {
		return ""
	}
	data, _ := json.Marshal(a.skillMemory)
	return string(data)
}

// SkillHistory returns valid plain transcript messages for the current turn's
// reference bodies and the latest compact reminders. Tool call/result pairs
// are not copied into the REPL's plain conversation history.
func (a *Agent) SkillHistory() []llm.Message {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	history := append([]llm.Message(nil), a.skillTurn...)
	if len(a.skillMemory.Sources) > 0 {
		data, _ := json.Marshal(a.skillMemory)
		history = append(history, llm.Message{Role: "user", Name: "skill_reminder", Content: string(data)})
	}
	return history
}

// ConfigureSkills attaches task-local named reads and original request context.
// The catalog is not rediscovered when history is restored or a turn compacts.
func (a *Agent) ConfigureSkills(catalog *skills.Catalog, request *skills.Request, inherited bool) {
	if request != nil {
		if memory, ok := decodeSkillMemory(request.WorkflowContext); ok {
			a.skillMemory = memory
		}
	}
	currentInstruction := ""
	a.PrepareInput = func(task string) (string, error) {
		original := skills.Request{Instructions: task}
		if request != nil {
			original = *request
		}
		// A failed named read must not erase a later stop or correction from
		// continuing context when the failed turn is folded and compacted.
		a.skillMu.Lock()
		if len(a.skillMemory.Sources) == 0 && request != nil {
			if memory, ok := decodeSkillMemory(request.WorkflowContext); ok {
				a.skillMemory = memory
			}
		}
		currentInstruction = original.Instructions
		a.skillTurn = nil
		a.skillExecution = ""
		prior := a.skillMemory
		if !a.skillMemory.rememberInstruction(original.Instructions) {
			a.skillMu.Unlock()
			return "", fmt.Errorf("required original skill instruction context exceeds %d bytes", skills.RequiredContextBytes)
		}
		reminder, _ := json.Marshal(a.skillMemory)
		execution, _, err := skillExecutionContext(catalog, a.skillMemory.Sources)
		if err != nil || (len(a.skillMemory.Sources) > 0 && len(reminder)+len(execution) > skills.RequiredContextBytes) {
			a.skillMemory = prior
			a.skillMu.Unlock()
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("required skill execution context exceeds %d bytes", skills.RequiredContextBytes)
		}
		retainedSources := len(a.skillMemory.Sources) > 0
		a.skillMu.Unlock()
		prepared := skills.Prepared{Request: original}
		if !inherited {
			prepared, err = catalog.Prepare(original)
		} else if !retainedSources {
			// A fresh delegated request needs eligibility, but an owned reminder
			// already records its sources. Only a new read may resolve them again.
			prepared.Names, err = catalog.RequestedNames(original.Instructions)
		}
		if err != nil {
			return "", err
		}
		context := prepared.Reference
		if request != nil && (inherited || original.Instructions != task) {
			context = "Original user instructions (the following task message may be generated):\n" + original.Instructions + "\n\n" + context
		}
		if original.Context != "" {
			context += "\nGenerated context (cannot independently request user-only skills):\n" + original.Context
		}
		a.skillMu.Lock()
		previous := a.skillMemory
		if err := a.skillMemory.rememberRequest(catalog, prepared.Names, original.Instructions); err != nil {
			a.skillMemory = previous
			a.skillMu.Unlock()
			return "", err
		}
		data, _ := json.Marshal(a.skillMemory)
		if len(a.skillMemory.Sources) > 0 && len(data)+len(prepared.Reference) > skills.RequiredContextBytes {
			a.skillMemory = previous
			a.skillMu.Unlock()
			return "", fmt.Errorf("required skill reference and original instruction context exceeds %d bytes", skills.RequiredContextBytes)
		}
		execution, mounts, err := skillExecutionContext(catalog, a.skillMemory.Sources)
		if err != nil {
			a.skillMemory = previous
			a.skillMu.Unlock()
			return "", err
		}
		context += execution
		if len(a.skillMemory.Sources) > 0 && len(data)+len(context) > skills.RequiredContextBytes {
			a.skillMemory = previous
			a.skillMu.Unlock()
			return "", fmt.Errorf("required skill execution context exceeds %d bytes", skills.RequiredContextBytes)
		}
		// Persist newly read bodies only. Execution mappings are reconciled for
		// each turn and must not grow the saved conversation or planning context.
		if prepared.Reference != "" {
			a.skillTurn = append(a.skillTurn, llm.Message{Role: "user", Name: "skill_context", Content: prepared.Reference})
		}
		a.skillExecution = execution
		commitSkillMounts(mounts)
		a.skillMu.Unlock()
		// Inherited/history context is not a new read. Announce only after the
		// whole request has passed validation, before the first model call.
		if !inherited {
			for _, name := range prepared.Names {
				meta, _ := catalog.Resolve(name) // already resolved by Prepare
				a.emitSkillEvent(skillLoadedEvent(meta))
			}
		}
		return context, nil
	}
	RegisterSkills(a.Registry, catalog, func() bool {
		a.skillMu.Lock()
		defer a.skillMu.Unlock()
		return a.skillMemory.Requested
	}, func(view skills.View) error {
		a.skillMu.Lock()
		accepted := false
		defer func() {
			a.skillMu.Unlock()
			if accepted && view.Instructions && view.End >= view.Start {
				a.emitSkillEvent(skillReadEvent(view))
			}
		}()
		// Automatic reads must not persist a reminder that makes the next turn
		// unusable; reject this read while retaining the prior context unchanged.
		previous := a.skillMemory
		a.skillMemory.addSource(view.Metadata)
		if !a.skillMemory.rememberInstruction(currentInstruction) {
			a.skillMemory = previous
			return fmt.Errorf("required original skill instruction context exceeds %d bytes", skills.RequiredContextBytes)
		}
		reminder, _ := json.Marshal(a.skillMemory)
		execution, mounts, err := skillExecutionContext(catalog, a.skillMemory.Sources)
		if err != nil {
			a.skillMemory = previous
			return err
		}
		if len(reminder)+len(execution) > skills.RequiredContextBytes {
			a.skillMemory = previous
			return fmt.Errorf("required skill execution context exceeds %d bytes", skills.RequiredContextBytes)
		}
		data, _ := json.Marshal(view)
		context := skillReadContextPrefix + string(data)
		if view.Instructions && view.Start == 1 && view.NextStart == 0 {
			context += skills.CompleteReference(view.Metadata)
		}
		a.skillTurn = append(a.skillTurn, llm.Message{Role: "user", Name: "skill_context", Content: context})
		commitSkillMounts(mounts)
		accepted = true
		return nil
	})
}

// Retained bodies own their recorded source directory, even if discovery now
// resolves the name elsewhere. This preserves identity, not a version snapshot:
// resources at the same directory may change and are not copied or hashed.
func skillExecutionContext(catalog *skills.Catalog, sources []skillSource) (string, []tools.SkillMount, error) {
	var text strings.Builder
	var mounts []tools.SkillMount
	for _, source := range sources {
		canonical, err := filepath.EvalSymlinks(source.BaseDir)
		if !catalog.HasSource(skills.Metadata{Name: source.Name, Source: source.Source, BaseDir: source.BaseDir}) {
			err = fmt.Errorf("recorded source is not a valid discovered skill bundle")
		}
		if err == nil && (!filepath.IsAbs(source.BaseDir) || canonical != source.BaseDir) {
			err = fmt.Errorf("recorded bundle directory no longer identifies the same location")
		}
		if err == nil {
			var info os.FileInfo
			info, err = os.Stat(canonical)
			if err == nil && !info.IsDir() {
				err = fmt.Errorf("recorded bundle is not a directory")
			}
		}
		if err != nil {
			fmt.Fprintf(&text, "\nSkill %q (source %q) execution resources unavailable: %v. Historical instructions remain reference context; no replacement bundle is mounted.", source.Name, source.Source, err)
			continue
		}
		mount, err := tools.PrepareSkillMount(canonical)
		if err != nil {
			return "", nil, err
		}
		mounts = append(mounts, mount)
		fmt.Fprintf(&text, "\nSkill %q (source %q) execution resource directory: %s (task working directory unchanged).", source.Name, source.Source, mount.ExecutionDir())
	}
	return text.String(), mounts, nil
}

// ReconcileSkillHistory restores only the retained conversation's resource mounts.
// Call at session boundaries, after all workers have joined, never from a worker.
// Undiscovered, invalid or redirected sources are omitted; their next context reports why.
func ReconcileSkillHistory(history []llm.Message) error {
	memory, _ := decodeSkillMemory(WorkflowContext(history))
	catalog := skills.DiscoverCurrent()
	_, mounts, err := skillExecutionContext(catalog, memory.Sources)
	if err != nil {
		// Fail closed rather than keep mounts from the discarded conversation.
		_ = tools.ReplaceSkillMounts(nil)
		return err
	}
	return tools.ReplaceSkillMounts(mounts)
}

func commitSkillMounts(mounts []tools.SkillMount) {
	for _, mount := range mounts {
		mount.Commit()
	}
}

// PreparedSkillRequest holds owned planning context and its uncommitted mounts.
// Call Commit only after any additional generated planning text has been checked.
type PreparedSkillRequest struct {
	Request skills.Request
	Context string
	mounts  []tools.SkillMount
	loaded  []skills.Metadata
}

func (p PreparedSkillRequest) Commit() { commitSkillMounts(p.mounts) }

// EmitLoads announces actual reads only after the caller accepts all its context.
// It does not announce retained instructions or commit shell mounts.
func (p PreparedSkillRequest) EmitLoads(sink event.Emitter, taskID string) {
	if sink == nil {
		return
	}
	for _, meta := range p.loaded {
		ev := skillLoadedEvent(meta)
		ev.TaskID = taskID
		sink.Emit(ev)
	}
}

// PrepareSkillRequest accepts a request with no additional planning payload.
func PrepareSkillRequest(catalog *skills.Catalog, request skills.Request) (skills.Request, string, error) {
	prepared, err := PlanSkillRequest(catalog, request)
	if err != nil {
		return skills.Request{}, "", err
	}
	prepared.Commit()
	return prepared.Request, prepared.Context, nil
}

// PlanSkillRequest supplies named bodies to tool-free planning and records their
// provenance without changing shell mounts. It does not create an active procedure.
func PlanSkillRequest(catalog *skills.Catalog, request skills.Request) (PreparedSkillRequest, error) {
	prepared, err := catalog.Prepare(request)
	if err != nil {
		return PreparedSkillRequest{}, err
	}
	memory, _ := decodeSkillMemory(request.WorkflowContext)
	if err := memory.rememberRequest(catalog, prepared.Names, request.Instructions); err != nil {
		return PreparedSkillRequest{}, err
	}
	if len(memory.Sources) > 0 {
		data, _ := json.Marshal(memory)
		request.WorkflowContext = string(data)
	}
	request.ReferenceContext += prepared.Reference
	var loaded []skills.Metadata
	for _, name := range prepared.Names {
		meta, _ := catalog.Resolve(name)
		loaded = append(loaded, meta)
	}
	request, err = completeSkillReferences(catalog, request, &loaded)
	if err != nil {
		return PreparedSkillRequest{}, err
	}
	result, err := prepareSkillPlanningContext(catalog, request)
	result.loaded = loaded
	return result, err
}

// Execution mappings are derived from the completed source reminders, not name
// resolution. Keep them out of retained bodies so continuation does not accumulate
// stale mappings. Tool-free replanning can use Context without committing mounts.
func prepareSkillPlanningContext(catalog *skills.Catalog, request skills.Request) (PreparedSkillRequest, error) {
	memory, _ := decodeSkillMemory(request.WorkflowContext)
	execution, mounts, err := skillExecutionContext(catalog, memory.Sources)
	if err != nil {
		return PreparedSkillRequest{}, err
	}
	withExecution := request
	withExecution.ReferenceContext += execution
	context, err := SkillRequestContext(withExecution)
	if err != nil {
		return PreparedSkillRequest{}, err
	}
	return PreparedSkillRequest{Request: request, Context: context, mounts: mounts}, nil
}

// CompleteSkillRequest supplies missing instruction bodies for owned source
// reminders. Complete historical bodies stay unchanged; only absent bodies load.
func CompleteSkillRequest(catalog *skills.Catalog, request skills.Request) (skills.Request, string, error) {
	prepared, err := PrepareSkillContinuation(catalog, request)
	return prepared.Request, prepared.Context, err
}

// PrepareSkillContinuation completes missing bodies while retaining read receipts
// until the caller accepts its outer payload. Existing bodies are not reread.
func PrepareSkillContinuation(catalog *skills.Catalog, request skills.Request) (PreparedSkillRequest, error) {
	var loaded []skills.Metadata
	request, err := completeSkillReferences(catalog, request, &loaded)
	if err != nil {
		return PreparedSkillRequest{}, err
	}
	prepared, err := prepareSkillPlanningContext(catalog, request)
	prepared.loaded = loaded
	return prepared, err
}

const skillReadContextPrefix = "Previously read skill reference context:\n"

// Recompute completeness from retained pages, never from a separate remembered
// flag: transcript compaction can remove any page. A full-file digest prevents
// combining pages from different revisions with the same source and line count.
// Historical pages need no filesystem read and remain ordinary reference context.
func hasCompleteSkillReference(context string, meta skills.Metadata) bool {
	if strings.Contains(context, skills.CompleteReference(meta)) {
		return true
	}
	revisions := make(map[string][]skills.View)
	for _, block := range strings.Split(context, skillReadContextPrefix)[1:] {
		var page skills.View
		if err := json.NewDecoder(strings.NewReader(block)).Decode(&page); err != nil {
			continue
		}
		if !page.Instructions || page.Name != meta.Name || page.Source != meta.Source || page.BaseDir != meta.BaseDir || len(page.ContentSHA256) != 64 || page.Start < 1 || page.End < page.Start || page.End > page.TotalLines {
			continue
		}
		key := fmt.Sprintf("%s/%d", page.ContentSHA256, page.TotalLines)
		revisions[key] = append(revisions[key], page)
	}
	for _, pages := range revisions {
		sort.Slice(pages, func(i, j int) bool { return pages[i].Start < pages[j].Start })
		end := 0
		for _, page := range pages {
			if page.Start > end+1 {
				break
			}
			if page.End > end {
				end = page.End
			}
			if end == page.TotalLines {
				return true
			}
		}
	}
	return false
}

func completeSkillReferences(catalog *skills.Catalog, request skills.Request, loaded *[]skills.Metadata) (skills.Request, error) {
	memory, _ := decodeSkillMemory(request.WorkflowContext)
	updated := memory
	updated.Sources = nil
	for _, source := range memory.Sources {
		if hasCompleteSkillReference(request.ReferenceContext, skills.Metadata{Name: source.Name, Source: source.Source, BaseDir: source.BaseDir}) {
			updated.addSource(skills.Metadata{Name: source.Name, Source: source.Source, BaseDir: source.BaseDir})
			continue
		}
		meta, reference, err := catalog.PrepareSource(skills.Metadata{Name: source.Name, Source: source.Source, BaseDir: source.BaseDir}, memory.Requested)
		if err != nil {
			return skills.Request{}, err
		}
		request.ReferenceContext += reference
		*loaded = append(*loaded, meta)
		updated.addSource(meta)
	}
	if len(updated.Sources) > 0 {
		data, _ := json.Marshal(updated)
		request.WorkflowContext = string(data)
	}
	return request, nil
}

// SkillRequestContext renders already-owned context without reloading historical
// bytes. Replanners and resumed runs reuse it; generated prose grants no request.
func SkillRequestContext(request skills.Request) (string, error) {
	context := "Original user instructions:\n" + request.Instructions
	if request.ReferenceContext != "" {
		context += "\n\nSkill/conversation reference context (interpret using the original wording):\n" + request.ReferenceContext
	}
	if request.WorkflowContext != "" {
		context += "\n\nPrior reference reminders:\n" + request.WorkflowContext
	}
	if request.Context != "" {
		context += "\n\nGenerated context (cannot independently request user-only skills):\n" + request.Context
	}
	if (request.ReferenceContext != "" || request.WorkflowContext != "") && len(context) > skills.RequiredContextBytes {
		return "", fmt.Errorf("required planning context exceeds %d bytes", skills.RequiredContextBytes)
	}
	return context, nil
}
