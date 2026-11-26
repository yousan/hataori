package claudecode

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/yousan/hataori/internal/agent"
)

// backgroundTask is one task of the background_tasks list, which a Stop
// carries and a StopFailure does not. A subagent's id is its agent_id.
type backgroundTask struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// subagentTask is the type of a subagent in that list.
const subagentTask = "subagent"

// task is what is known of one background task.
type task struct {
	running  bool // false once its end was learned
	subagent bool // and not a shell or a monitor
}

// tasks is what the codec knows of the session's background tasks, by the
// CLI's task id. An end is kept until the next list, because the report of
// a task's start may be read after it. It is what a turn's end that comes
// without a list counts: a failed, interrupted or blocked turn's, none of
// which ends the tasks.
type tasks map[string]task

// list takes a list the CLI gave, which names every task that runs.
func (t *tasks) list(l []backgroundTask) {
	*t = tasks{}
	for _, b := range l {
		if b.Status == "running" {
			(*t)[b.ID] = task{running: true, subagent: b.Type == subagentTask}
		}
	}
}

// at returns the turn's end or request e with the tasks that run then, as
// far as is known: how many, and the subagents among them.
func (t tasks) at(e agent.Event) agent.Event {
	e.Background, e.Subagents = 0, nil
	for id, b := range t {
		if b.running {
			e.Background++
			if b.subagent {
				e.Subagents = append(e.Subagents, id)
			}
		}
	}
	sort.Strings(e.Subagents)
	return e
}

// tool notes the result of a tool of the main agent's that says it started a
// task in the background, a subagent, a shell or a monitor, resumed a
// subagent that had ended, or stopped a task.
func (t *tasks) tool(name string, response json.RawMessage) {
	var r struct {
		IsAsync bool   `json:"isAsync"`
		AgentID string `json:"agentId"`
		Shell   string `json:"backgroundTaskId"`
		Resumed string `json:"resumedAgentId"`
		Task    string `json:"taskId"` // a Monitor's; other tools name tasks of other sorts with it
		Stopped string `json:"task_id"`
	}
	json.Unmarshal(response, &r) // a tool whose response is another shape starts none
	var id string
	b := task{running: true}
	switch {
	case name == "TaskStop":
		t.end(r.Stopped)
	case name == "Monitor":
		id = r.Task
	case r.IsAsync:
		id, b.subagent = r.AgentID, true
	case r.Resumed != "":
		id, b.subagent = r.Resumed, true
	default:
		id = r.Shell
	}
	if id == "" {
		return
	}
	if _, known := (*t)[id]; known {
		return
	}
	if *t == nil {
		*t = tasks{}
	}
	(*t)[id] = b
}

// resumed notes that the subagent id starts. One whose end was learned runs
// again: it was resumed with a message. Of any other, only the result of the
// tool that started it says whether it runs in the background.
func (t tasks) resumed(id string) {
	if _, known := t[id]; known {
		t[id] = task{running: true, subagent: true}
	}
}

// end notes that the task id ended.
func (t *tasks) end(id string) {
	if id == "" {
		return
	}
	if *t == nil {
		*t = tasks{}
	}
	(*t)[id] = task{}
}

// notificationTag begins the prompt of the turn the CLI starts when a
// background task ended, and the transcript line that says it did.
const notificationTag = "<task-notification>"

// notified notes the end a task notification gives, if text is one. One
// without a status says that something happened in the task, not that it
// ended.
func (t *tasks) notified(text string) {
	if !strings.HasPrefix(strings.TrimSpace(text), notificationTag) {
		return
	}
	switch tagged(text, "status") {
	case "", "running":
		return
	}
	t.end(tagged(text, "task-id"))
}

// tagged returns the content of the first element name in text.
func tagged(text, name string) string {
	_, rest, ok := strings.Cut(text, "<"+name+">")
	if !ok {
		return ""
	}
	s, _, ok := strings.Cut(rest, "</"+name+">")
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}
