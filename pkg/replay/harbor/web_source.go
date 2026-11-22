package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type indexedWebAction struct {
	step int
	args json.RawMessage
}

// webSource pairs external completion events by action and recorded order.
// Harbor ATIF v1.7 omits the external action ID and its response contents.
type webSource struct {
	actions []indexedWebAction
	seen    map[string]bool
	next    int
}

func splitWebActions(steps []sourceStep, allowed bool) ([]sourceStep, *webSource, error) {
	web := &webSource{seen: make(map[string]bool)}
	ordinary := make([]sourceStep, 0, len(steps))
	for _, source := range steps {
		if len(source.Tools) != len(source.Observation.Results) {
			return nil, nil, fmt.Errorf("unpaired ATIF tools and observations at step %d", source.ID)
		}
		step := source
		step.Tools = source.Tools[:0:0]
		step.Observation.Results = source.Observation.Results[:0:0]
		for i, call := range source.Tools {
			result := source.Observation.Results[i]
			if call.Name != "web_search_call" {
				step.Tools = append(step.Tools, call)
				step.Observation.Results = append(step.Observation.Results, result)
				continue
			}
			if !allowed || call.ID != "" || result.ID != "" || result.Content != "" {
				return nil, nil, fmt.Errorf("unsupported external web action or result at step %d", source.ID)
			}
			web.actions = append(web.actions, indexedWebAction{step: source.ID, args: call.Args})
		}
		ordinary = append(ordinary, step)
	}
	return ordinary, web, nil
}

func (w *webSource) bind(line []byte) (step int, id string, err error) {
	if w.done() {
		return 0, "", errors.New("unexpected completed external web action")
	}
	var row struct {
		Type string `json:"type"`
		Item struct {
			Type   string                     `json:"type"`
			ID     string                     `json:"id"`
			Status string                     `json:"status"`
			Action map[string]json.RawMessage `json:"action"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return 0, "", err
	}
	item := row.Item
	if row.Type != "response_item" || item.Type != "web_search_call" || item.Status != "completed" ||
		!strings.HasPrefix(item.ID, "ws_") || len(item.ID) == 3 || w.seen[item.ID] {
		return 0, "", errors.New("incomplete or duplicate native external web action")
	}
	var kind string
	if err := json.Unmarshal(item.Action["type"], &kind); err != nil {
		return 0, "", err
	}
	if kind != "search" && kind != "open_page" && kind != "find_in_page" {
		return 0, "", fmt.Errorf("unsupported external action type %q", kind)
	}
	if _, duplicate := item.Action["action_type"]; duplicate {
		return 0, "", errors.New("ambiguous native external action type")
	}
	item.Action["action_type"] = item.Action["type"]
	delete(item.Action, "type")
	match := w.actions[w.next]
	if kind == "find_in_page" {
		var pattern string
		if err := json.Unmarshal(item.Action["pattern"], &pattern); err != nil || pattern == "" {
			return 0, "", errors.New("missing native find-in-page pattern")
		}
		var projected map[string]json.RawMessage
		if err := json.Unmarshal(match.args, &projected); err != nil {
			return 0, "", err
		}
		if _, included := projected["pattern"]; !included {
			// ATIF v1.7 retains the URL and omits the native search pattern.
			delete(item.Action, "pattern")
		}
	}
	args, err := json.Marshal(item.Action)
	if err != nil {
		return 0, "", err
	}
	if !equalJSON(args, match.args) {
		return 0, "", fmt.Errorf("external web action differs from ATIF step %d", match.step)
	}
	w.seen[item.ID] = true
	w.next++
	return match.step, item.ID, nil
}

func (w *webSource) done() bool {
	return w.next == len(w.actions)
}
