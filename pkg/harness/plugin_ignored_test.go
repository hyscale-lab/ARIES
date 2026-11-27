package harness

import (
	"encoding/json"
	"reflect"
	"testing"
)

// plugins.json records what a package's manifest declared that ARIES ignored.
func TestPluginRecordListsWhatWasIgnored(t *testing.T) {
	plugin := testPlugin()
	plugin.Ignored = []string{`unknown plugin.json field "commands"`}
	record, err := PluginRecord([]Plugin{plugin})
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(record, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded[0]["ignored"], []any{`unknown plugin.json field "commands"`}) {
		t.Fatalf("record = %s", record)
	}
}
