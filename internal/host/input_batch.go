package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	InputBatchVersion  = "input-batch-v1"
	MaxInputBatches    = 128
	MaxInputBatchBytes = 128 * 1024
)

// InputCapability is live receiver evidence, not a path, PID or spec file.
type InputCapability struct {
	Version string `json:"version"`
	Run     string `json:"run"`
}

type InputBatch struct {
	Run  string   `json:"run"`
	Key  string   `json:"key"`
	Data [][]byte `json:"data,omitempty"`
}

// InputReceipt confirms a receiver write only. Agent input acknowledgement
// remains separate. A reserved/error key never becomes executable again.
type InputReceipt struct {
	State       string `json:"state"` // absent, pending, written, failed
	Digest      string `json:"digest,omitempty"`
	Error       string `json:"error,omitempty"`
	CompletedAt int64  `json:"completed_at,omitempty"`
}

func InputBatchDigest(data [][]byte) string {
	b, _ := json.Marshal(data)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (h *host) inputCapability() response {
	if h.spec.Relay || h.input == nil || h.run == "" {
		return response{Error: "idempotent input is unsupported by this host"}
	}
	select {
	case <-h.done:
		return response{Error: "the command has exited"}
	default:
	}
	return response{OK: true, InputCapability: &InputCapability{Version: InputBatchVersion, Run: h.run}}
}

func (h *host) inputBatch(b *InputBatch, lookup bool) response {
	if b == nil || b.Run == "" || b.Run != h.run || b.Key == "" || len(b.Key) > 128 || h.spec.Relay || h.input == nil {
		return response{Error: "idempotent input capability or run does not match"}
	}
	h.batchMu.Lock()
	if r, ok := h.batches[b.Key]; ok {
		if !lookup && r.Digest != InputBatchDigest(b.Data) {
			h.batchMu.Unlock()
			return response{Error: "idempotent input key has different bytes"}
		}
		h.batchMu.Unlock()
		return response{OK: true, InputReceipt: &r}
	}
	if lookup {
		h.batchMu.Unlock()
		return response{OK: true, InputReceipt: &InputReceipt{State: "absent"}}
	}
	var size int
	for _, chunk := range b.Data {
		size += len(chunk)
	}
	if len(b.Data) == 0 || size == 0 || size > MaxInputBatchBytes || len(b.Data) > 128 || len(h.batches) >= MaxInputBatches {
		h.batchMu.Unlock()
		return response{Error: "idempotent input capacity or payload limit reached"}
	}
	if h.batches == nil {
		h.batches = make(map[string]InputReceipt)
	}
	r := InputReceipt{State: "pending", Digest: InputBatchDigest(b.Data)}
	h.batches[b.Key] = r
	h.batchMu.Unlock()

	// Reserve before the first effect. Connection loss cannot cancel it.
	// All chunks stay under the same lock as ordinary input, in order.
	h.inMu.Lock()
	var err error
	select {
	case <-h.done:
		err = errors.New("the command has exited")
	default:
		for _, chunk := range b.Data {
			n, e := h.input.Write(chunk)
			if e != nil {
				err = e
				break
			}
			if n != len(chunk) {
				err = io.ErrShortWrite
				break
			}
		}
	}
	h.inMu.Unlock()
	r.State, r.CompletedAt = "written", time.Now().UnixMilli()
	if err != nil {
		r.State, r.Error = "failed", "input write failed or was partial"
	}
	h.batchMu.Lock()
	h.batches[b.Key] = r
	h.batchMu.Unlock()
	return response{OK: true, InputReceipt: &r}
}

func (c *Client) InputCapability() (InputCapability, error) {
	r, err := c.call(request{Op: "input-capability"})
	if err != nil {
		return InputCapability{}, err
	}
	if r.InputCapability == nil || r.InputCapability.Version != InputBatchVersion || r.InputCapability.Run == "" {
		return InputCapability{}, errors.New("host: idempotent input is unsupported")
	}
	return *r.InputCapability, nil
}

func (c *Client) InputBatch(b InputBatch) (InputReceipt, error) {
	return c.inputBatch(b, false)
}

func (c *Client) InputReceipt(run, key string) (InputReceipt, error) {
	return c.inputBatch(InputBatch{Run: run, Key: key}, true)
}

func (c *Client) inputBatch(b InputBatch, lookup bool) (InputReceipt, error) {
	op := "input-batch"
	if lookup {
		op = "input-receipt"
	}
	r, err := c.call(request{Op: op, Batch: &b})
	if err != nil {
		return InputReceipt{}, err
	}
	if r.InputReceipt == nil {
		return InputReceipt{}, errors.New("host: missing input receipt")
	}
	return *r.InputReceipt, nil
}
