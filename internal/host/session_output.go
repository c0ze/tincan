package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Output consumes every stdout byte before Run's bounded capture can truncate
// it. Only assistant text is returned as progress: provider/plugin inventories,
// system prompts, thought events and authentication metadata stay out of MCP.
func (s *SessionRun) Output(stream string, data []byte) ([]byte, error) {
	if s.Preset.Session != "persistent" {
		return data, nil
	}
	if stream != "stdout" {
		// Verbose hooks may put inventories or configuration on stderr. Keep
		// that stream in the private host log; terminal failures retain details.
		return nil, nil
	}
	return s.parser.write(data)
}

// Reply flushes the structured stream and extracts the final assistant answer.
// Process failures preserve the session pointer and never trigger a fresh run.
func (s *SessionRun) Reply(spec RunSpec, res Result) (string, error) {
	if s.Preset.Session == "persistent" && res.ExitCode != 0 && res.Err == nil && !res.Killed && !res.TimedOut {
		// Providers often report the only useful failure detail as a structured
		// stdout result and also exit nonzero. Preserve that parsed error, while
		// keeping raw startup/plugin metadata out of the reply.
		_ = s.parser.flush()
		if s.parser.failure != "" {
			return "", fmt.Errorf("provider failed (exit=%d): %s", res.ExitCode, s.parser.failure)
		}
	}
	if res.Err != nil || res.ExitCode != 0 || res.Killed || res.TimedOut {
		// Avoid returning captured stdout inventories inside an error result.
		if s.Preset.Session == "persistent" {
			res.Stdout = nil
		}
		return PostProcess(s.record.Provider, ReplyBody(spec, res)), nil
	}
	if s.Preset.Session != "persistent" {
		body := ReplyBody(spec, res)
		if strings.TrimSpace(body) == "" {
			return "", errors.New("agent returned an empty reply")
		}
		return body, nil
	}
	if err := s.parser.flush(); err != nil {
		return "", err
	}
	if s.parser.failure != "" {
		return "", fmt.Errorf("provider failed: %s", s.parser.failure)
	}
	if !s.record.Ready {
		return "", errors.New("provider returned no session ID; initialization remains pending and requires explicit reset")
	}
	if !s.parser.final || len(bytes.TrimSpace(s.parser.body)) == 0 {
		return "", errors.New("provider returned no parseable final answer")
	}
	return strings.TrimSpace(string(s.parser.body)), nil
}

type sessionParser struct {
	provider  string
	onSession func(string) error
	pending   []byte
	body      []byte
	failure   string
	final     bool
	err       error
}

func (p *sessionParser) write(data []byte) ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	var progress bytes.Buffer
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		n := len(data)
		if i >= 0 {
			n = i
		}
		if len(p.pending)+n > MaxCapturedOutput {
			p.err = errors.New("provider JSON event exceeds output limit")
			return nil, p.err
		}
		p.pending = append(p.pending, data[:n]...)
		if i < 0 {
			break
		}
		text, err := p.event(bytes.TrimSpace(p.pending))
		p.pending = p.pending[:0]
		if err != nil {
			p.err = err
			return nil, err
		}
		progress.WriteString(text)
		data = data[n+1:]
	}
	return progress.Bytes(), nil
}

func (p *sessionParser) flush() error {
	if p.err != nil {
		return p.err
	}
	if len(bytes.TrimSpace(p.pending)) > 0 {
		_, p.err = p.event(bytes.TrimSpace(p.pending))
	}
	p.pending = nil
	return p.err
}

type jsonFields map[string]json.RawMessage

func fieldString(m jsonFields, key string) string {
	var v string
	_ = json.Unmarshal(m[key], &v)
	return v
}

func object(raw json.RawMessage) jsonFields {
	var m jsonFields
	_ = json.Unmarshal(raw, &m)
	return m
}

func textContent(raw json.RawMessage) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var blocks []jsonFields
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if fieldString(block, "type") == "text" {
			b.WriteString(fieldString(block, "text"))
		}
	}
	return b.String()
}

func (p *sessionParser) identify(id string) error {
	if id == "" {
		return nil
	}
	if p.onSession == nil {
		return nil
	}
	return p.onSession(id)
}

func (p *sessionParser) setBody(body string, appendText bool) error {
	if appendText {
		if len(p.body)+len(body) > MaxCapturedOutput {
			return errors.New("provider final answer exceeds output limit")
		}
		p.body = append(p.body, body...)
	} else {
		if len(body) > MaxCapturedOutput {
			return errors.New("provider final answer exceeds output limit")
		}
		p.body = append(p.body[:0], body...)
	}
	return nil
}

func (p *sessionParser) event(line []byte) (string, error) {
	if len(line) == 0 {
		return "", nil
	}
	var m jsonFields
	if err := json.Unmarshal(line, &m); err != nil || m == nil {
		return "", errors.New("provider emitted invalid JSON output")
	}
	kind := fieldString(m, "type")
	if kind == "error" || fieldString(m, "event") == "error" {
		p.failure = providerError(m)
	}
	switch p.provider {
	case "claude":
		if err := p.identify(fieldString(m, "session_id")); err != nil {
			return "", err
		}
		switch kind {
		case "assistant":
			return textContent(object(m["message"])["content"]), nil
		case "result":
			var failed bool
			_ = json.Unmarshal(m["is_error"], &failed)
			if failed || (fieldString(m, "subtype") != "" && fieldString(m, "subtype") != "success") {
				p.failure = providerError(m)
			}
			p.final = true
			return "", p.setBody(fieldString(m, "result"), false)
		}
	case "grok":
		if err := p.identify(fieldString(m, "sessionId")); err != nil {
			return "", err
		}
		switch kind {
		case "text":
			text := fieldString(m, "data")
			return text, p.setBody(text, true)
		case "tool_call", "tool_use":
			// Commentary before tool execution is not the final answer.
			p.body = p.body[:0]
		case "end":
			stop := fieldString(m, "stopReason")
			if stop != "end_turn" && stop != "stop_sequence" {
				p.failure = "incomplete response: " + stop
			}
			p.final = true
		}
	case "agy":
		if err := p.identify(fieldString(m, "conversation_id")); err != nil {
			return "", err
		}
		switch fieldString(m, "event") {
		case "step_update":
			step := object(m["step_update"])
			if fieldString(step, "step_type") == "agent_response" {
				return fieldString(step, "text_delta"), nil
			}
		case "result":
			result := object(m["result"])
			if err := p.identify(fieldString(result, "conversation_id")); err != nil {
				return "", err
			}
			if fieldString(result, "status") != "SUCCESS" {
				p.failure = providerError(result)
			}
			p.final = true
			return "", p.setBody(fieldString(result, "response"), false)
		}
	case "kimi":
		if kind == "session.resume_hint" {
			if err := p.identify(fieldString(m, "session_id")); err != nil {
				return "", err
			}
			p.final = true
		}
		if fieldString(m, "role") == "assistant" {
			text := textContent(m["content"])
			if text != "" {
				return text, p.setBody(text, false)
			}
		}
	}
	return "", nil
}

func providerError(m jsonFields) string {
	for _, key := range []string{"message", "error", "result", "response"} {
		if s := fieldString(m, key); s != "" {
			return s
		}
	}
	if s := fieldString(object(m["error"]), "message"); s != "" {
		return s
	}
	if s := fieldString(m, "subtype"); s != "" {
		return s
	}
	if s := fieldString(m, "status"); s != "" {
		return s
	}
	return "provider reported an error"
}
