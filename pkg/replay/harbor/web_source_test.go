package harbor

import (
	"encoding/json"
	"testing"
)

func TestWebSourceBindsCompletedActionWithoutTurningItIntoAGuestCall(t *testing.T) {
	var steps []sourceStep
	if err := json.Unmarshal([]byte(`[{"step_id":4,"tool_calls":[
 {"function_name":"web_search_call","tool_call_id":"","arguments":{"action_type":"search","query":"sample","queries":["sample"]}},
 {"function_name":"exec_command","tool_call_id":"call_1","arguments":{"cmd":"pwd"}}
],"observation":{"results":[{},{"source_call_id":"call_1","content":"workdir"}]}}]`), &steps); err != nil {
		t.Fatal(err)
	}
	ordinary, web, err := splitWebActions(steps, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary) != 1 || len(ordinary[0].Tools) != 1 ||
		ordinary[0].Tools[0].ID != "call_1" || ordinary[0].Observation.Results[0].ID != "call_1" {
		t.Fatalf("ordinary tool binding changed: %+v", ordinary)
	}
	if web.done() {
		t.Fatal("external action is lost")
	}
	line := []byte(`{"type":"response_item","payload":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"sample","queries":["sample"]}}}`)
	step, id, err := web.bind(line)
	if err != nil || step != 4 || id != "ws_1" || !web.done() {
		t.Fatalf("external completion mismatch: %d %s %v", step, id, err)
	}
	if _, _, err := web.bind(line); err == nil {
		t.Fatal("duplicate external completion accepted")
	}
	if _, _, err := splitWebActions(steps, false); err == nil {
		t.Fatal("external action accepted for unreviewed task")
	}
}

func TestExternalWebActionRequiresTheSourceArguments(t *testing.T) {
	web := &webSource{seen: map[string]bool{}, actions: []indexedWebAction{
		{6, json.RawMessage(`{"action_type":"open_page"}`)},
	}}
	bad := []byte(`{"type":"response_item","payload":{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"open_page","url":"https://example.com"}}}`)
	if _, _, err := web.bind(bad); err == nil || web.done() {
		t.Fatal("external action with substituted URL was accepted")
	}
	good := []byte(`{"type":"response_item","payload":{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"open_page"}}}`)
	if _, _, err := web.bind(good); err != nil || !web.done() {
		t.Fatalf("matching recorded omission rejected: %v", err)
	}
}

func TestFindInPageBindsAnATIFProjectionWithoutTheNativePattern(t *testing.T) {
	web := &webSource{seen: map[string]bool{}, actions: []indexedWebAction{
		{9, json.RawMessage(`{"action_type":"find_in_page","url":"https://example.com"}`)},
	}}
	bad := []byte(`{"type":"response_item","payload":{"type":"web_search_call","id":"ws_3","status":"completed","action":{"type":"find_in_page","url":"https://example.com"}}}`)
	if _, _, err := web.bind(bad); err == nil || web.done() {
		t.Fatal("incomplete native find action was accepted")
	}
	good := []byte(`{"type":"response_item","payload":{"type":"web_search_call","id":"ws_3","status":"completed","action":{"type":"find_in_page","url":"https://example.com","pattern":"example"}}}`)
	if step, id, err := web.bind(good); err != nil || step != 9 || id != "ws_3" || !web.done() {
		t.Fatalf("matching find completed action rejected: %d %s %v", step, id, err)
	}
}
