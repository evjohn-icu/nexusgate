package credentials

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCredentialMarshalJSONRedactsAPIKey(t *testing.T) {
	c := Credential{
		BaseURL:        "https://api.example.com",
		APIKey:         "sk-super-secret-do-not-leak",
		Model:          "model-v1",
		AuthHeader:     "Authorization",
		AuthScheme:     "Bearer",
		TimeoutSeconds: 30,
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal(Credential): %v", err)
	}

	if strings.Contains(string(raw), "sk-super-secret-do-not-leak") {
		t.Fatalf("MarshalJSON leaked API key in output: %s", raw)
	}
	if !strings.Contains(string(raw), `"[redacted]"`) {
		t.Fatalf("MarshalJSON did not redact api_key: %s", raw)
	}
}

func TestCredentialMarshalJSONEmptyKey(t *testing.T) {
	c := Credential{
		BaseURL: "https://api.example.com",
		APIKey:  "",
		Model:   "model-v1",
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal(Credential): %v", err)
	}

	// Empty APIKey should stay empty, not become "[redacted]".
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	apiKey, ok := decoded["api_key"]
	if !ok {
		t.Fatal("api_key field missing from output")
	}
	if apiKey != "" {
		t.Fatalf("empty api_key should remain empty, got %v", apiKey)
	}
}

func TestCredentialFormatDoesNotLeakAPIKey(t *testing.T) {
	c := Credential{
		BaseURL: "https://api.example.com",
		APIKey:  "sk-super-secret-do-not-leak",
		Model:   "model-v1",
	}

	for _, tc := range []struct {
		name string
		out  string
	}{
		{name: "%v", out: fmt.Sprintf("%v", c)},
		{name: "%+v", out: fmt.Sprintf("%+v", c)},
		{name: "%s", out: fmt.Sprintf("%s", c)},
		{name: "%#v", out: fmt.Sprintf("%#v", c)},
	} {
		if strings.Contains(tc.out, "sk-super-secret-do-not-leak") {
			t.Errorf("%s leaked API key: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "[redacted]") {
			t.Errorf("%s did not redact API key: %s", tc.name, tc.out)
		}
	}
}

func TestLeaseFormatDoesNotLeakCredentialAPIKey(t *testing.T) {
	l := Lease{
		JobID:     "job-1",
		WorkerID:  "worker-1",
		Provider:  "gemini",
		Operation: OperationVideoAnalysis,
		ExpiresAt: "2026-08-01T00:00:00Z",
		Credential: Credential{
			BaseURL: "https://vision.example.com",
			APIKey:  "sk-leaked-via-lease",
			Model:   "gemini-pro-vision",
		},
	}

	out := fmt.Sprintf("%+v", l)
	if strings.Contains(out, "sk-leaked-via-lease") {
		t.Fatalf("%%+v on Lease leaked API key: %s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("%%+v on Lease did not redact credential.api_key: %s", out)
	}
}

func TestLeaseJSONRedactsCredentialAPIKey(t *testing.T) {
	l := Lease{
		JobID:     "job-1",
		WorkerID:  "worker-1",
		Provider:  "gemini",
		Operation: OperationVideoAnalysis,
		ExpiresAt: "2026-08-01T00:00:00Z",
		Credential: Credential{
			BaseURL: "https://vision.example.com",
			APIKey:  "sk-leaked-via-lease",
			Model:   "gemini-pro-vision",
		},
	}

	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("Marshal(Lease): %v", err)
	}

	if strings.Contains(string(raw), "sk-leaked-via-lease") {
		t.Fatalf("Marshal(Lease) leaked API key: %s", raw)
	}
	if !strings.Contains(string(raw), `"[redacted]"`) {
		t.Fatalf("Marshal(Lease) did not redact credential.api_key: %s", raw)
	}
	if !strings.Contains(string(raw), `"gemini-pro-vision"`) {
		t.Fatal("Marshal(Lease) lost non-sensitive credential fields")
	}
}
