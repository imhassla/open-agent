# Open-agent

Open-agent carries out user tasks with workers and reusable skills. These terms describe how skills are discovered and selected for a user's work.

## Language

**Skill**:
A reusable set of task instructions, optionally accompanied by supporting references, scripts, or assets.
_Avoid_: Plugin, tool

**Skill catalog**:
A collection of available skills and their descriptions, used to discover which procedures may help with a task.

**Skill inventory**:
A user's view of discovered skills and the diagnostics that explain their availability. It includes user-only skills.

**Explicit invocation**:
A user's instruction to use a particular skill, including an instruction saved in a scheduled task. Its position within the instruction does not change its meaning.

**Automatic selection**:
A worker's choice of a relevant skill without the user having requested that particular skill.

**User-only skill**:
A skill available through explicit invocation or as a named helper of an explicitly requested skill workflow, and excluded from independent automatic selection.
_Avoid_: Disabled skill

**Skill selection**:
The association of a skill with a user's continuing workflow. Remembering a selection does not mean its full instructions are always present in context.
