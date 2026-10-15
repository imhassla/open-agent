package agent

import (
	"fmt"

	"github.com/imhassla/open-agent/internal/event"
	"github.com/imhassla/open-agent/internal/skills"
)

func skillLoadedEvent(meta skills.Metadata) event.Event {
	return event.Event{Kind: "skill_load", Text: "skill loaded: /" + meta.Name, SkillName: meta.Name, SkillSource: meta.Source}
}

func skillReadEvent(view skills.View) event.Event {
	ev := skillLoadedEvent(view.Metadata)
	if view.Start != 1 || view.NextStart != 0 {
		ev.Kind = "skill_read"
		ev.Text = fmt.Sprintf("skill read: /%s (lines %d-%d of %d; partial)", view.Name, view.Start, view.End, view.TotalLines)
	}
	return ev
}

func (a *Agent) emitSkillEvent(ev event.Event) {
	ev.TaskID, ev.Model = a.Label, a.Model
	a.emit(ev)
}
