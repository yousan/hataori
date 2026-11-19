package codex

import "encoding/json"

type message struct {
	EmittedAtMS *int64          `json:"emittedAtMs"`
	ID          json.RawMessage `json:"id,omitempty"`
	Method      string          `json:"method,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *wireError      `json:"error,omitempty"`
}
type wireError struct {
	Message string `json:"message"`
}
type wireTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  struct {
		CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
	} `json:"error"`
}
type wireItem struct {
	ID                string              `json:"id"`
	Tool              string              `json:"tool"`
	Server            string              `json:"server"`
	Kind              string              `json:"kind"`
	AgentThreadID     string              `json:"agentThreadId"`
	AgentPath         string              `json:"agentPath"`
	ReceiverThreadIDs []string            `json:"receiverThreadIds"`
	Type              string              `json:"type"`
	Text              string              `json:"text"`
	Phase             string              `json:"phase"`
	Delivery          string              `json:"delivery"`
	Questions         []asyncWireQuestion `json:"questions"`
	Changes           []struct {
		Path string `json:"path"`
	} `json:"changes"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}
type params struct {
	ThreadID string   `json:"threadId"`
	Turn     wireTurn `json:"turn"`
	Item     wireItem `json:"item"`
}
type result struct {
	Thread *struct {
		ID string `json:"id"`
	} `json:"thread"`
	Turn   *wireTurn `json:"turn"`
	Model  string    `json:"model"`
	Effort string    `json:"reasoningEffort"`
}
