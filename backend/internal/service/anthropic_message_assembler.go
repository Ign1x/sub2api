package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type anthropicBufferedError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicMessageAssembly struct {
	message       map[string]json.RawMessage
	blocks        []*anthropicBufferedBlock
	started       bool
	terminal      bool
	upstreamError *anthropicBufferedError
}

type anthropicBufferedBlock struct {
	fields    map[string]json.RawMessage
	strings   map[string]*strings.Builder
	input     strings.Builder
	hasInput  bool
	citations []json.RawMessage
	closed    bool
}

// Preserve raw fields, including opaque thinking signatures, server tool
// results, citations and provider metadata. Reconstruct only fields delivered
// as deltas; no conversion through another provider's message schema occurs.
func assembleAnthropicMessage(sse []byte) (*anthropicMessageAssembly, error) {
	a := &anthropicMessageAssembly{}
	scanner := bufio.NewScanner(bytes.NewReader(sse))
	scanner.Buffer(make([]byte, 4096), len(sse)+1)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return a.event([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return a, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return a, err
	}
	if err := flush(); err != nil {
		return a, err
	}
	if !a.started || !a.terminal {
		return a, fmt.Errorf("missing message_start or message_stop")
	}
	for _, block := range a.blocks {
		if !block.closed {
			return a, fmt.Errorf("content block was not closed")
		}
	}
	return a, nil
}

func (a *anthropicMessageAssembly) event(payload []byte) error {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		if a.started && rawAnthropicString(a.message["stop_reason"]) != "" {
			a.terminal = true
		}
		return nil
	}
	var e struct {
		Type         string                     `json:"type"`
		Index        *int                       `json:"index"`
		Message      map[string]json.RawMessage `json:"message"`
		ContentBlock map[string]json.RawMessage `json:"content_block"`
		Delta        map[string]json.RawMessage `json:"delta"`
		Usage        map[string]json.RawMessage `json:"usage"`
		Error        *anthropicBufferedError    `json:"error"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return err
	}
	if e.Type == "ping" {
		return nil
	}
	if e.Type == "error" {
		a.upstreamError = e.Error
		return fmt.Errorf("upstream error event")
	}
	if a.terminal {
		return fmt.Errorf("event after message_stop")
	}
	if !a.started && e.Type != "message_start" {
		return fmt.Errorf("event before message_start")
	}
	switch e.Type {
	case "message_start":
		if a.started || rawAnthropicString(e.Message["type"]) != "message" {
			return fmt.Errorf("invalid or repeated message_start")
		}
		a.started, a.message = true, e.Message
		var initial []map[string]json.RawMessage
		if content := e.Message["content"]; len(content) > 0 {
			if err := json.Unmarshal(content, &initial); err != nil {
				return err
			}
		}
		for _, block := range initial {
			a.blocks = append(a.blocks, &anthropicBufferedBlock{fields: block, closed: true})
		}
	case "content_block_start":
		if e.Index == nil || *e.Index != len(a.blocks) || e.ContentBlock == nil {
			return fmt.Errorf("invalid content block start index")
		}
		a.blocks = append(a.blocks, &anthropicBufferedBlock{fields: e.ContentBlock, strings: make(map[string]*strings.Builder)})
	case "content_block_delta", "content_block_stop":
		if e.Index == nil || *e.Index < 0 || *e.Index >= len(a.blocks) || a.blocks[*e.Index].closed {
			return fmt.Errorf("invalid content block index or lifecycle")
		}
		block := a.blocks[*e.Index]
		if e.Type == "content_block_stop" {
			block.closed = true
			return block.finish()
		}
		return block.delta(e.Delta)
	case "message_delta":
		for key, value := range e.Delta {
			a.message[key] = value
		}
		if len(e.Usage) > 0 {
			var usage map[string]json.RawMessage
			_ = json.Unmarshal(a.message["usage"], &usage)
			if usage == nil {
				usage = make(map[string]json.RawMessage)
			}
			mergeAnthropicRawObject(usage, e.Usage)
			a.message["usage"], _ = json.Marshal(usage)
		}
	case "message_stop":
		if rawAnthropicString(a.message["stop_reason"]) == "" {
			return fmt.Errorf("message_stop without stop_reason")
		}
		a.terminal = true
	}
	return nil
}

func rawAnthropicString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func mergeAnthropicRawObject(dst, src map[string]json.RawMessage) {
	for key, value := range src {
		var left, right map[string]json.RawMessage
		if json.Unmarshal(dst[key], &left) == nil && left != nil && json.Unmarshal(value, &right) == nil && right != nil {
			mergeAnthropicRawObject(left, right)
			value, _ = json.Marshal(left)
		}
		dst[key] = value
	}
}

func (b *anthropicBufferedBlock) delta(delta map[string]json.RawMessage) error {
	var field string
	switch kind := rawAnthropicString(delta["type"]); kind {
	case "text_delta":
		field = "text"
	case "thinking_delta":
		field = "thinking"
	case "signature_delta":
		field = "signature"
	case "compaction_delta":
		field = "content"
	case "input_json_delta":
		var part string
		if err := json.Unmarshal(delta["partial_json"], &part); err != nil {
			return err
		}
		b.hasInput = true
		b.input.WriteString(part)
		return nil
	case "citations_delta":
		if !json.Valid(delta["citation"]) {
			return fmt.Errorf("invalid citation delta")
		}
		if b.citations == nil {
			_ = json.Unmarshal(b.fields["citations"], &b.citations)
		}
		b.citations = append(b.citations, delta["citation"])
		return nil
	default:
		return fmt.Errorf("unsupported content delta %q", kind)
	}
	var part string
	if err := json.Unmarshal(delta[field], &part); err != nil {
		return err
	}
	if b.strings[field] == nil {
		b.strings[field] = &strings.Builder{}
		b.strings[field].WriteString(rawAnthropicString(b.fields[field]))
	}
	b.strings[field].WriteString(part)
	return nil
}

func (b *anthropicBufferedBlock) finish() error {
	for field, value := range b.strings {
		b.fields[field], _ = json.Marshal(value.String())
	}
	if b.hasInput {
		input := []byte(b.input.String())
		var object map[string]json.RawMessage
		if err := json.Unmarshal(input, &object); err != nil || object == nil {
			return fmt.Errorf("invalid tool input JSON")
		}
		b.fields["input"] = input
	}
	if b.citations != nil {
		b.fields["citations"], _ = json.Marshal(b.citations)
	}
	return nil
}

func (a *anthropicMessageAssembly) json() ([]byte, error) {
	content := make([]map[string]json.RawMessage, 0, len(a.blocks))
	for _, block := range a.blocks {
		content = append(content, block.fields)
	}
	a.message["content"], _ = json.Marshal(content)
	return json.Marshal(a.message)
}
