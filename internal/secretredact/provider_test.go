package secretredact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderRedactor(t *testing.T) {
	const longKey = "provider-key-123456789"
	const headerSecret = "header-secret-987654"
	r := New(Material{
		APIKey:       longKey,
		ExtraHeaders: map[string]string{"X-Provider-Secret": headerSecret, "X-Ordinary": "ordinary"},
		BaseURL:      "https://url-user:url-password@provider.example/v1?token=url-token&mode=keep#SECRET=url-fragment",
	})
	tests := []struct {
		name   string
		body   string
		want   []string
		absent []string
	}{
		{"long and unrelated", `{"answer":"` + longKey + `","ordinary":"ordinary-value","header":"` + headerSecret + `"}`, []string{marker, "ordinary-value"}, []string{longKey, headerSecret}},
		{"short only in credential key", `{"note":"tiny","api_key":"tiny","nested":{"TOKEN":"tiny"}}`, []string{`"api_key":"` + marker + `"`, `"TOKEN":"` + marker + `"`, `"note":"tiny"`}, []string{"\"note\":\"" + marker}},
		{"nested escaped json", `{"payload":"{\"authorization\":\"Bearer ` + longKey + `\",\"ok\":\"ordinary-value\"}"}`, []string{marker, "ordinary-value"}, []string{longKey}},
		{"urls", `{"url":"https://url-user:url-password@provider.example/v1?TOKEN=url-token&mode=keep#secret=url-fragment"}`, []string{"mode=keep", "provider.example"}, []string{"url-user", "url-password", "url-token", "url-fragment"}},
		{"empty values", `{"token":"","ordinary":""}`, []string{`"token":""`}, []string{marker}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := r.JSON([]byte(tc.body))
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			if !json.Valid(out) {
				t.Fatalf("invalid output JSON: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(string(out), absent) {
					t.Errorf("output %q contains %q", out, absent)
				}
			}
		})
	}
}

func TestProviderRedactorTextAndURL(t *testing.T) {
	r := New(Material{APIKey: "short", ExtraHeaders: map[string]string{"X-Key": "header-long-secret"}, BaseURL: "https://user:pass@x.example"})
	out := string(r.Text([]byte("Bearer short api_key=short X=header-long-secret ordinary=shorter")))
	if strings.Contains(out, " short ") || strings.Contains(out, "header-long-secret") {
		t.Fatalf("text leaked credential: %s", out)
	}
	if !strings.Contains(out, "ordinary=shorter") {
		t.Fatalf("text changed ordinary value: %s", out)
	}
	u := r.URL("https://user:pass@x.example/p?API_KEY=abc&keep=yes#ToKeN=def&x=1")
	if strings.Contains(u, "user") || strings.Contains(u, "pass") || strings.Contains(strings.ToLower(u), "api_key") || strings.Contains(strings.ToLower(u), "token") || !strings.Contains(u, "keep=yes") {
		t.Fatalf("URL sanitization failed: %s", u)
	}
}

func TestProviderRedactorArbitraryExtraHeaderAndURLPostProcessing(t *testing.T) {
	const headerSecret = "arbitrary-header-secret-123"
	const querySecret = "query credential / 123"
	r := New(Material{
		ExtraHeaders: map[string]string{"X-Custom-Provider": headerSecret},
		BaseURL:      "https://url-user:url-password@example.invalid/v1?relay=" + strings.ReplaceAll(querySecret, " ", "%20") + "&keep=yes#note=keep",
	})
	out, err := r.JSON([]byte(`{"header":"` + headerSecret + `","url":"https://url-user:url-password@example.invalid/v1?relay=query%20credential%20%2F%20123&keep=yes#note=keep"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), headerSecret) || strings.Contains(string(out), querySecret) || strings.Contains(string(out), "url-user") || strings.Contains(string(out), "url-password") {
		t.Fatalf("credential leaked: %s", out)
	}
	if !strings.Contains(string(out), marker) || !strings.Contains(string(out), "keep=yes") || !strings.Contains(string(out), "note=keep") {
		t.Fatalf("redaction or ordinary URL values missing: %s", out)
	}
}

func TestProviderRedactorInvalidJSON(t *testing.T) {
	if _, err := New(Material{APIKey: "long-secret"}).JSON([]byte(`{"broken":`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
