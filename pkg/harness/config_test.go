package harness

import (
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestSearchEndpointRequirements(t *testing.T) {
	for _, address := range []string{"", "search.example:8123", "file:///tmp/search", "http://user:secret@search.example", "http://search.example/?key=secret", "http://search.example/#fragment", "http://search.example/\n"} {
		if ValidateSearch(address, true) == nil {
			t.Errorf("accepted %q", address)
		}
		if ValidateSearch(address, false) != nil {
			t.Errorf("disabled search required endpoint %q", address)
		}
	}
	for _, address := range []string{"http://search.example:8123", "https://search.example/base", "http://[::1]:8123"} {
		if err := ValidateSearch(address, true); err != nil {
			t.Errorf("rejected %q: %v", address, err)
		}
	}
}

// URL edge cases live here; native renderer tests verify the normalized output.
func TestNormalizeV1BaseURL(t *testing.T) {
	for _, invalid := range []string{"http://host/v1/v1", "http://host/v1?", "http://host/v%31", "http://:30000/v1", "https://[]/v1"} {
		if _, err := NormalizeV1BaseURL(invalid); err == nil {
			t.Errorf("accepted base URL %q", invalid)
		}
	}
	if got, err := NormalizeV1BaseURL("http://host/v1/"); err != nil || got != "http://host/v1" {
		t.Fatalf("normalized URL = %q, error = %v", got, err)
	}
}

func TestRunIDValidation(t *testing.T) {
	for _, id := range []string{"", "-bad", "bad/name"} {
		if err := ValidateRunID("Harness", id); err == nil {
			t.Fatalf("ValidateRunID(%q) succeeded", id)
		}
	}
}

func TestValidateModelRequiresHostname(t *testing.T) {
	for _, provider := range []string{"deepseek", "openai", "sglang"} {
		for _, host := range []string{":30000", "[]", "localhost:30000", "[::1]:30000"} {
			model := core.ModelConfig{Provider: provider, BaseURL: "http://" + host + "/v1", Model: "test-model", APIKeyEnv: "MODEL_KEY"}
			err := ValidateModel("Harness", model)
			invalid := host == ":30000" || host == "[]"
			if (err != nil) != invalid {
				t.Errorf("provider %s host %q: error = %v", provider, host, err)
			}
		}
	}
}
