package agentplugin

import (
	"reflect"
	"strings"
	"testing"
)

// §5.2 and §8.1: an unknown field and a non-object extensions field are
// reported and ignored; the package still loads.
func TestLoadReportsWhatItIgnores(t *testing.T) {
	files := validPackage()
	files["plugin.json"] = strings.Replace(manifest, `}`, `, "commands": ["x"], "extensions": 1}`, 1)
	plugin, err := Load(writePackage(t, files), "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plugin.json extensions, which is not an object", `unknown plugin.json field "commands"`}
	if !reflect.DeepEqual(plugin.Ignored, want) {
		t.Fatalf("ignored = %q, want %q", plugin.Ignored, want)
	}
}
