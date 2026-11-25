package claudecode

import (
	"encoding/json"
	"strings"
)

// backgroundTask is one task of the background_tasks list, which a Stop
// carries and a StopFailure does not. A subagent's id is its agent_id.
type backgroundTask struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// tasks is what the codec knows of the session's background tasks, by the
// CLI's task id: true while a task runs, false once its end was learned.
// The end is kept until the next list, because the report of a task's start
// may be read after it. It is what a turn's end that comes without a list
// counts: a failed turn's, and an interrupted one's, neither of which ends
// the tasks.
type tasks map[string]bool

// list takes a list the CLI gave, which names every task that runs, and
// returns how many do.
func (t *tasks) list(l []backgroundTask) int {
	*t = tasks{}
	n := 0
	for _, b := range l {
		if b.Status == "running" {
			(*t)[b.ID] = true
			n++
		}
	}
	return n
}

// running is the number of tasks that run, as far as is known.
func (t tasks) running() int {
	n := 0
	for _, r := range t {
		if r {
			n++
		}
	}
	return n
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
	switch {
	case name == "TaskStop":
		t.end(r.Stopped)
	case name == "Monitor":
		id = r.Task
	case r.IsAsync:
		id = r.AgentID
	case r.Resumed != "":
		id = r.Resumed
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
	(*t)[id] = true
}

// resumed notes that the subagent id starts. One whose end was learned runs
// again: it was resumed with a message. Of any other, only the result of the
// tool that started it says whether it runs in the background.
func (t tasks) resumed(id string) {
	if _, known := t[id]; known {
		t[id] = true
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
	(*t)[id] = false
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
