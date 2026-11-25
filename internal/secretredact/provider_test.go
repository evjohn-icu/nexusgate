package secretredact

import (
	"encoding/json"
	"errors"
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
		// Configured API key and secret header values are redacted at every
		// occurrence under the stricter policy — including inside the value of
		// an ordinary header named in the same credential block, and in the
		// rewritten key name.
		{"long and unrelated", `{"answer":"` + longKey + `","ordinary":"ordinary-value","header":"` + headerSecret + `"}`, []string{marker, `"answer":"` + marker + `"`}, []string{longKey, headerSecret, "ordinary-value", "ordinary"}},
		{"short only in credential key", `{"note":"tiny","api_key":"tiny","nested":{"TOKEN":"tiny"}}`, []string{`"api_key":"` + marker + `"`, `"TOKEN":"` + marker + `"`, `"note":"tiny"`}, []string{"\"note\":\"" + marker}},
		{"nested escaped json", `{"payload":"{\"authorization\":\"Bearer ` + longKey + `\",\"ok\":\"untouched-value\"}"}`, []string{marker, "untouched-value"}, []string{longKey, "ordinary-value"}},
		{"urls", `{"url":"https://url-user:url-password@provider.example/v1?TOKEN=url-token&mode=keep#secret=url-fragment"}`, []string{"mode=keep", "provider.example"}, []string{"url-user", "url-password", "url-token", "url-fragment"}},
		{"empty values", `{"token":"","unrelated":""}`, []string{`"token":""`, `"unrelated":""`}, []string{marker}},
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

// The stricter JSON policy: configured API keys and secret header values are
// redacted at every occurrence, in keys and values, including their
// URL-encoded spellings; anything that cannot be sanitized safely withholds
// the whole response with the generic sentinel.
func TestRedactorJSONRedactsEveryOccurrenceOfPrimaryMaterial(t *testing.T) {
	const apiKey = "abc def"
	r := New(Material{APIKey: apiKey, ExtraHeaders: map[string]string{"X-Secret": "{{SECRET_sj2k3l9d}}"}})
	out, err := r.JSON([]byte(`{"note":"x` + apiKey + `y","url":"https://h.example/?k=abc%20def","nested":"{\"deep\":\"abc+def\"}","z":"plain"}`))
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	text := string(out)
	for _, material := range []string{apiKey, "abc def", "abc%20def", "abc+def", "{{SECRET_sj2k3l9d}}"} {
		if strings.Contains(text, material) {
			t.Fatalf("output leaked %q: %s", material, text)
		}
	}
	for _, want := range []string{`"note":"x` + marker + `y"`, `?k=` + marker, `{\"deep\":\"` + marker + `\"}`, `"z":"plain"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %s missing %q", text, want)
		}
	}
}

// A short configured API key must sanitize in every JSON position, not only
// inside established credential context.
func TestRedactorJSONShortKeyEveryPosition(t *testing.T) {
	r := New(Material{APIKey: "abc"})
	out, err := r.JSON([]byte(`{"note":"xabcz","arr":["abc","tail"],"obj":{"k":"abc"}}`))
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "abc") && !strings.Contains(text, `"`+marker+`"`) {
		t.Fatalf("short key survived outside credential context: %s", text)
	}
	if strings.Count(text, marker) != 3 {
		t.Fatalf("expected three redactions, got %s", text)
	}
}

func TestRedactorJSONWithholdsUnredactableResponses(t *testing.T) {
	t.Run("material inside a number", func(t *testing.T) {
		const apiKey = "12345"
		r := New(Material{APIKey: apiKey})
		out, err := r.JSON([]byte(`{"count":123450}`))
		if !errors.Is(err, ErrRedactionWithheld) {
			t.Fatalf("error = %v, want ErrRedactionWithheld", err)
		}
		if out != nil {
			t.Fatalf("withheld response must not be returned, got %s", out)
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("error leaked material: %v", err)
		}
	})
	t.Run("key rewrite collision", func(t *testing.T) {
		r := New(Material{APIKey: "X"})
		if _, err := r.JSON([]byte(`{"aXc":1,"a[REDACTED]c":2}`)); !errors.Is(err, ErrRedactionWithheld) {
			t.Fatalf("error = %v, want ErrRedactionWithheld", err)
		}
	})
	t.Run("material inside the marker", func(t *testing.T) {
		r := New(Material{APIKey: "REDACTED"})
		if _, err := r.JSON([]byte(`{"ok":true}`)); !errors.Is(err, ErrRedactionWithheld) {
			t.Fatalf("error = %v, want ErrRedactionWithheld", err)
		}
	})
}

// The exact-replacement routine is single-pass: the marker it inserts is never
// rescanned, so a pre-existing marker in the response cannot be corrupted and
// no source text is revisited.
func TestRedactorJSONNeverReprocessesMarker(t *testing.T) {
	const apiKey = "sk-test-123"
	r := New(Material{APIKey: apiKey})
	out, err := r.JSON([]byte(`{"log":"prev=[REDACTED] now=` + apiKey + `"}`))
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if got, want := string(out), `{"log":"prev=[REDACTED] now=`+marker+`"}`; got != want {
		t.Fatalf("output = %s, want %s", got, want)
	}
}

// Validation must also catch the JSON-escaped spelling of a secret that the
// exact pass cannot rewrite because the encoder escaped it.
func TestRedactorJSONWithholdsEscapedSurvivor(t *testing.T) {
	const apiKey = `a"b`
	r := New(Material{APIKey: apiKey})
	// The secret reaches the encoder through a numeric position is impossible;
	// instead the nested stringify path preserves it with escapes when the
	// exact pass cannot see the decoded form. Sanitize must either redact it
	// or withhold — never emit it.
	out, err := r.JSON([]byte(`{"k":"a\"b"}`))
	if err != nil {
		if !errors.Is(err, ErrRedactionWithheld) {
			t.Fatalf("error = %v, want ErrRedactionWithheld", err)
		}
		return
	}
	if strings.Contains(string(out), `a\"b`) {
		t.Fatalf("escaped secret survived: %s", out)
	}
}
