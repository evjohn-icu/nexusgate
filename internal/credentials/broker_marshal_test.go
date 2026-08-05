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

func TestCredentialMarshalJSONRedactsExtraHeaders(t *testing.T) {
	c := Credential{
		BaseURL: "https://api.example.com",
		APIKey:  "sk-secret",
		Model:   "model-v1",
		ExtraHeaders: map[string]string{
			"Authorization":       "Bearer sk-real-token-value",
			"X-Api-Key":           "sk-another-secret",
			"x-api-key":           "lowercase-secret",
			"Api-Key":             "dash-secret",
			"Proxy-Authorization": "Basic encoded-creds",
			"Content-Type":        "application/json",
			"X-Request-Id":        "req-123",
		},
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal(Credential): %v", err)
	}

	output := string(raw)

	for _, secret := range []string{"sk-real-token-value", "sk-another-secret", "lowercase-secret", "dash-secret", "encoded-creds"} {
		if strings.Contains(output, secret) {
			t.Fatalf("MarshalJSON leaked ExtraHeaders secret %q in: %s", secret, output)
		}
	}

	if cnt := strings.Count(output, "[redacted]"); cnt < 6 {
		t.Fatalf("expected >=6 [redacted] occurrences, got %d: %s", cnt, output)
	}

	if !strings.Contains(output, "extra_headers") {
		t.Fatal("extra_headers still present but values should be safe")
	}
}

func TestCredentialFormatRedactsExtraHeaders(t *testing.T) {
	c := Credential{
		BaseURL: "https://api.example.com",
		APIKey:  "sk-secret",
		Model:   "model-v1",
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer real-token",
		},
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
		if strings.Contains(tc.out, "real-token") {
			t.Errorf("%s leaked ExtraHeaders secret: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "[redacted]") {
			t.Errorf("%s did not redact secrets: %s", tc.name, tc.out)
		}
	}
}

func TestLeaseAuditAPIKeyNotSerializable(t *testing.T) {
	audit := LeaseAudit{
		JobID:     "job-1",
		WorkerID:  "worker-1",
		Provider:  "gemini",
		Operation: OperationVideoAnalysis,
		APIKey:    "if-json-tag-is-wrong-this-leaks",
		BaseURL:   "https://evil.example",
	}

	raw, err := json.Marshal(audit)
	if err != nil {
		t.Fatalf("Marshal(LeaseAudit): %v", err)
	}

	output := string(raw)
	if strings.Contains(output, "if-json-tag-is-wrong-this-leaks") {
		t.Fatalf("LeaseAudit.APIKey leaked via JSON: %s", output)
	}
	if strings.Contains(output, "api_key") {
		t.Fatalf("LeaseAudit JSON still contains api_key field: %s", output)
	}
	if strings.Contains(output, "https://evil.example") && strings.Contains(output, "base_url") {
		t.Fatalf("LeaseAudit.BaseURL leaked via JSON: %s", output)
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

// P1: URL-embedded credentials must be stripped in MarshalJSON / Format / GoString.
func TestCredentialMarshalJSONSanitizesBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string // substring that must appear
		absent  string // substring that must NOT appear
	}{
		{
			name:    "userinfo in URL",
			baseURL: "https://user:secret@api.example.com/v1",
			want:    "api.example.com",
			absent:  "user:secret",
		},
		{
			name:    "api_key query param",
			baseURL: "https://api.example.com/v1?api_key=sk-leaked&other=val",
			want:    "other=val",
			absent:  "sk-leaked",
		},
		{
			name:    "key query param",
			baseURL: "https://api.example.com/v1?key=abcdef&mode=test",
			want:    "mode=test",
			absent:  "abcdef",
		},
		{
			name:    "token query param",
			baseURL: "https://api.example.com/v1?token=secret123",
			want:    "api.example.com",
			absent:  "secret123",
		},
		{
			name:    "secret query param",
			baseURL: "https://api.example.com/v1?secret=mysecret&foo=bar",
			want:    "foo=bar",
			absent:  "mysecret",
		},
		{
			name:    "password query param",
			baseURL: "https://api.example.com/v1?password=p@ss&x=1",
			want:    "x=1",
			absent:  "p@ss",
		},
		{
			name:    "clean URL unchanged",
			baseURL: "https://api.example.com/v1/chat",
			want:    "https://api.example.com/v1/chat",
			absent:  "",
		},
		{
			name:    "empty URL",
			baseURL: "",
			want:    "",
			absent:  "",
		},
		{
			name:    "API_KEY case-insensitive removed",
			baseURL: "https://api.example.com/v1?API_KEY=sk-leaked&x=1",
			want:    "x=1",
			absent:  "sk-leaked",
		},
		{
			name:    "fragment token stripped",
			baseURL: "https://api.example.com/path?mode=1#token=abc",
			want:    "mode=1",
			absent:  "token=abc",
		},
		{
			name:    "apikey param removed",
			baseURL: "https://api.example.com/v1?apikey=secret&x=1",
			want:    "x=1",
			absent:  "secret",
		},
		{
			name:    "access_token param removed",
			baseURL: "https://api.example.com/v1?access_token=secret&x=1",
			want:    "x=1",
			absent:  "secret",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Credential{
				BaseURL: tc.baseURL,
				APIKey:  "sk-key",
				Model:   "model-v1",
			}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			output := string(raw)
			if tc.want != "" && !strings.Contains(output, tc.want) {
				t.Errorf("expected output to contain %q, got: %s", tc.want, output)
			}
			if tc.absent != "" && strings.Contains(output, tc.absent) {
				t.Errorf("output must NOT contain %q, got: %s", tc.absent, output)
			}
		})
	}
}

// P1: URL sanitization must also work through Format / GoString paths.
func TestCredentialFormatSanitizesBaseURL(t *testing.T) {
	c := Credential{
		BaseURL: "https://user:pass@api.example.com/path?api_key=sk-leaked&mode=1",
		APIKey:  "sk-secret",
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
		if strings.Contains(tc.out, "user:pass") {
			t.Errorf("%s leaked userinfo: %s", tc.name, tc.out)
		}
		if strings.Contains(tc.out, "sk-leaked") {
			t.Errorf("%s leaked query api_key: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "mode=1") {
			t.Errorf("%s lost non-sensitive query param: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "api.example.com") {
			t.Errorf("%s lost hostname: %s", tc.name, tc.out)
		}
	}
}

// P2: LeaseAudit.Format / GoString must prevent APIKey and BaseURL leaks
// through %v, %+v, %#v, %s formatting.
func TestLeaseAuditFormatDoesNotLeakCredentials(t *testing.T) {
	audit := LeaseAudit{
		JobID:     "job-1",
		WorkerID:  "worker-1",
		Provider:  "gemini",
		Operation: OperationVideoAnalysis,
		APIKey:    "sk-hand-crafted-leak",
		BaseURL:   "https://evil.example?api_key=bad",
	}

	for _, tc := range []struct {
		name string
		out  string
	}{
		{name: "%v", out: fmt.Sprintf("%v", audit)},
		{name: "%+v", out: fmt.Sprintf("%+v", audit)},
		{name: "%s", out: fmt.Sprintf("%s", audit)},
		{name: "%#v", out: fmt.Sprintf("%#v", audit)},
	} {
		if strings.Contains(tc.out, "sk-hand-crafted-leak") {
			t.Errorf("%s leaked APIKey: %s", tc.name, tc.out)
		}
		if strings.Contains(tc.out, "https://evil.example") {
			t.Fatalf("%s leaked BaseURL: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "[redacted]") {
			t.Errorf("%s did not redact credentials: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "job-1") {
			t.Errorf("%s lost non-sensitive fields: %s", tc.name, tc.out)
		}
	}
}

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "userinfo removed",
			input: "https://user:pass@host/path",
			want:  "https://host/path",
		},
		{
			name:  "api_key removed",
			input: "https://host/path?api_key=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "only sensitive params removed",
			input: "https://host/path?key=s&token=t&secret=s2&password=p&mode=test",
			want:  "https://host/path?mode=test",
		},
		{
			name:  "clean URL unchanged",
			input: "https://host/path?mode=test",
			want:  "https://host/path?mode=test",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "invalid URL returned as-is",
			input: "not-a-url",
			want:  "not-a-url",
		},
		{
			name:  "API_KEY case-insensitive removed",
			input: "https://host/path?API_KEY=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "ApiKey camelCase removed",
			input: "https://host/path?ApiKey=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "apikey param removed",
			input: "https://host/path?apikey=secret&mode=test",
			want:  "https://host/path?mode=test",
		},
		{
			name:  "access_token param removed",
			input: "https://host/path?access_token=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "api_key_id param removed",
			input: "https://host/path?api_key_id=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "credential param removed",
			input: "https://host/path?credential=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "authorization param removed",
			input: "https://host/path?authorization=secret&x=1",
			want:  "https://host/path?x=1",
		},
		{
			name:  "fragment token param removed",
			input: "https://host/path?mode=test#token=abc",
			want:  "https://host/path?mode=test",
		},
		{
			name:  "fragment access_token removed but non-sensitive kept",
			input: "https://host/path?mode=test#access_token=secret&page=1",
			want:  "https://host/path?mode=test#page=1",
		},
		{
			name:  "truly malformed URL returns placeholder",
			input: "http://[::1%25",
			want:  "[invalid-url]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeURL(tc.input)
			if got != tc.want {
				t.Fatalf("sanitizeURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
