package harbor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WildClawSource is a native OpenClaw session timeline. Its tool payloads are
// untrusted source data. Parsing alone establishes neither provenance nor replay admission.
type WildClawSource struct {
	Version                                      int
	SHA256, SessionID, RunID, TaskID, Model, CWD string
	Status, StatusReason                         string
	Score                                        float64
	Start, End                                   time.Time
	Records                                      []WildClawSourceRecord
	Batches                                      []WildClawSourceBatch
}

// WildClawSourceRecord preserves the file row and its native timeline metadata.
type WildClawSourceRecord struct {
	Line          int
	ID            string    `json:"id"`
	ParentID      string    `json:"parentId"`
	Type          string    `json:"type"`
	Time          time.Time `json:"timestamp"`
	Role          string
	StopReason    string
	MessageTimeMS *int64
}

// WildClawSourceBatch contains the calls in their order within one assistant message.
type WildClawSourceBatch struct {
	Request WildClawSourceRecord
	Calls   []WildClawSourceCall
}

type WildClawSourceCall struct {
	Sequence     uint64
	ContentIndex int // Zero-based position in the native assistant content array.
	ID, Name     string
	Arguments    json.RawMessage
	Reply        *WildClawSourceReply // A nil reply marks an unanswered request.
}

type WildClawSourceReply struct {
	Record           WildClawSourceRecord
	IsError          *bool
	Status           string // String or numeric status from Details, as text.
	Content, Details json.RawMessage
}

type wildClawSourceRow struct {
	WildClawSourceRecord
	Version      int
	Harness      string
	CWD          string
	Model        string
	RunID        string `json:"run_id"`
	TaskID       string `json:"task_id"`
	Score        float64
	Status       string `json:"trace_status"`
	StatusReason string `json:"trace_status_reason"`
	Message      *struct {
		Role       string
		StopReason string `json:"stopReason"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    *bool  `json:"isError"`
		Timestamp  *int64
		Content    json.RawMessage
		Details    json.RawMessage
	}
}

// ParseWildClawSource reads the linear OpenClaw v3 JSONL session format used by
// WildClawBench. It indexes requests and replies without interpreting tool arguments.
func ParseWildClawSource(trace []byte) (*WildClawSource, error) {
	lines := bytes.Split(bytes.TrimSuffix(trace, []byte("\n")), []byte("\n"))
	var header wildClawSourceRow
	if err := json.Unmarshal(lines[0], &header); err != nil || header.Type != "session" ||
		header.Harness != "openclaw" || header.Version != 3 || strings.TrimSpace(header.ID) == "" ||
		header.RunID == "" || header.TaskID == "" || header.Time.IsZero() || header.ParentID != "" ||
		header.Message != nil || (header.Status != "completed" && header.Status != "interrupted") {
		return nil, errors.New("invalid OpenClaw session header at line 1")
	}
	source := &WildClawSource{
		Version: header.Version, SHA256: digest(trace), SessionID: header.ID,
		RunID: header.RunID, TaskID: header.TaskID, Model: header.Model, CWD: header.CWD,
		Status: header.Status, StatusReason: header.StatusReason, Score: header.Score,
		Records: []WildClawSourceRecord{{Line: 1, ID: header.ID, Type: header.Type, Time: header.Time}},
	}
	records := map[string]bool{header.ID: true}
	calls := make(map[string]*WildClawSourceCall)
	previous := header.Time
	var tip string // The native metadata chain can start at its own root after the header.
	unanswered := 0
	for i, line := range lines[1:] {
		n := i + 2
		var row wildClawSourceRow
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("invalid OpenClaw row at line %d", n)
		}
		if strings.TrimSpace(row.ID) == "" || records[row.ID] ||
			(row.ParentID != tip && !(i == 0 && row.ParentID == header.ID)) {
			return nil, fmt.Errorf("invalid OpenClaw record identity or parent at line %d", n)
		}
		if row.Type == "" || row.Type == "session" || row.Time.IsZero() ||
			row.Time.Before(previous) || !source.End.IsZero() {
			return nil, fmt.Errorf("invalid OpenClaw timeline at line %d", n)
		}
		records[row.ID] = true
		tip, previous = row.ID, row.Time
		m := row.Message
		if (row.Type == "message") != (m != nil) {
			return nil, fmt.Errorf("invalid OpenClaw message envelope at line %d", n)
		}
		record := WildClawSourceRecord{Line: n, ID: row.ID, ParentID: row.ParentID, Type: row.Type, Time: row.Time}
		if m != nil {
			record.Role, record.StopReason, record.MessageTimeMS = m.Role, m.StopReason, m.Timestamp
		}
		source.Records = append(source.Records, record)
		if m == nil {
			continue
		}

		var content []struct {
			Type, ID, Name string
			Arguments      json.RawMessage
		}
		if err := json.Unmarshal(m.Content, &content); err != nil || content == nil {
			return nil, fmt.Errorf("invalid OpenClaw message content at line %d", n)
		}
		batch := WildClawSourceBatch{Request: record}
		batchIDs := make(map[string]bool)
		for j, block := range content {
			if block.Type == "" {
				return nil, fmt.Errorf("invalid OpenClaw content block at line %d, content index %d", n, j)
			}
			if block.Type != "toolCall" {
				continue
			}
			if m.Role != "assistant" || m.StopReason != "toolUse" ||
				strings.TrimSpace(block.ID) == "" || strings.TrimSpace(block.Name) == "" ||
				calls[block.ID] != nil || batchIDs[block.ID] || !bytes.HasPrefix(block.Arguments, []byte("{")) {
				return nil, fmt.Errorf("invalid OpenClaw tool call at line %d, content index %d", n, j)
			}
			batchIDs[block.ID] = true
			batch.Calls = append(batch.Calls, WildClawSourceCall{
				Sequence: uint64(len(calls) + len(batch.Calls) + 1), ContentIndex: j,
				ID: block.ID, Name: block.Name, Arguments: block.Arguments,
			})
		}

		switch {
		case m.Role == "user" && source.Start.IsZero():
			source.Start = row.Time
		case source.Start.IsZero():
			return nil, fmt.Errorf("OpenClaw message precedes the user prompt at line %d", n)
		case m.Role == "assistant" && m.StopReason == "toolUse":
			if unanswered != 0 || len(batch.Calls) == 0 {
				return nil, fmt.Errorf("invalid OpenClaw request batch at line %d", n)
			}
			for j := range batch.Calls {
				call := &batch.Calls[j]
				calls[call.ID] = call
			}
			unanswered = len(batch.Calls)
			source.Batches = append(source.Batches, batch)
		case m.Role == "toolResult":
			call := calls[m.ToolCallID]
			if call == nil || call.Reply != nil || m.ToolName != call.Name {
				return nil, fmt.Errorf("invalid OpenClaw tool reply identity at line %d", n)
			}
			call.Reply = &WildClawSourceReply{
				Record: record, IsError: m.IsError, Status: wildClawReplyStatus(m.Details),
				Content: m.Content, Details: m.Details,
			}
			unanswered--
		case m.Role == "assistant" && m.StopReason == "stop":
			source.End = row.Time
		default:
			return nil, fmt.Errorf("unsupported OpenClaw message at line %d", n)
		}
	}
	if source.Start.IsZero() || (source.Status == "completed" && (source.End.IsZero() || unanswered > 0)) {
		return nil, errors.New("incomplete OpenClaw session timeline")
	}
	return source, nil
}

func wildClawReplyStatus(details json.RawMessage) string {
	var d struct{ Status json.RawMessage }
	if json.Unmarshal(details, &d) != nil || len(d.Status) == 0 {
		return ""
	}
	var label string
	if json.Unmarshal(d.Status, &label) == nil {
		return label
	}
	var code json.Number
	if json.Unmarshal(d.Status, &code) == nil {
		return string(code)
	}
	return ""
}
