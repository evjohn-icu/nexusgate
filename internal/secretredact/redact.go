// Package secretredact removes provider credentials from data returned across
// a boundary. It is intentionally independent of the credential and HTTP
// packages so the same rules can be used by logs and proxy responses.
package secretredact

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const marker = "[REDACTED]"

// ErrRedactionWithheld is the generic error returned when redaction cannot
// guarantee that no configured credential material remains in the output —
// because a key rewrite collided with another key, because replacement would
// corrupt the marker itself, or because material survived redaction and
// re-encoding in a position the sanitizer cannot rewrite. The caller must
// withhold the whole response; the error deliberately carries no material.
var ErrRedactionWithheld = errors.New("redaction cannot guarantee configured credentials were removed; response withheld")

type Material struct {
	APIKey       string
	ExtraHeaders map[string]string
	BaseURL      string
}

// Redactor carries the configured credential material for one provider
// endpoint. Secrets are held only in memory and never appear in errors,
// markers or diagnostics produced by this package.
type Redactor struct {
	secrets  []string
	primary  map[string]bool
	short    map[string]bool
	tokens   []redactToken
	withheld bool
}

// redactToken is one exact byte sequence the sanitizer searches for: a raw
// secret or one of its URL-encoded forms. Primary tokens come from the
// configured API key and secret header values and are redacted at every
// occurrence; the rest keep the boundary and short-value context rules.
type redactToken struct {
	value   string
	primary bool
	short   bool
}

var sensitiveKey = map[string]bool{
	"authorization": true, "api_key": true, "apikey": true, "token": true,
	"secret": true, "password": true, "access_token": true, "credential": true,
	"key": true,
}

func New(m Material) Redactor {
	r := Redactor{primary: make(map[string]bool), short: make(map[string]bool)}
	seen := make(map[string]bool)
	add := func(s string, primary bool) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		r.secrets = append(r.secrets, s)
		r.primary[s] = primary
		r.short[s] = len([]byte(s)) < 8
	}
	// Configured API keys and secret header values are the operator's own
	// credential material: every occurrence must go, wherever it turns up.
	add(m.APIKey, true)
	for _, s := range m.ExtraHeaders {
		// Extra headers are write-only credential configuration. Header names are
		// not a reliable indication of what a relay expects in their values.
		add(s, true)
	}
	if u, err := url.Parse(m.BaseURL); err == nil {
		if u.User != nil {
			if p, ok := u.User.Password(); ok && p != "" {
				add(p, false)
			}
			add(u.User.Username(), false)
		}
		for name, values := range u.Query() {
			for _, value := range values {
				if sensitiveKey[strings.ToLower(name)] || len([]byte(value)) >= 8 {
					add(value, false)
				}
			}
		}
		if fragment, err := url.ParseQuery(u.Fragment); err == nil {
			for name, values := range fragment {
				for _, value := range values {
					if sensitiveKey[strings.ToLower(name)] || len([]byte(value)) >= 8 {
						add(value, false)
					}
				}
			}
		}
	}
	r.tokens = buildTokens(r.secrets, r.primary, r.short)
	// A token inside the marker would be re-matched inside the replacement and
	// corrupt every marker the sanitizer emits. Withholding is the only safe
	// answer, and JSON applies it from the first call on.
	for _, t := range r.tokens {
		if strings.Contains(marker, t.value) {
			r.withheld = true
			break
		}
	}
	return r
}

// buildTokens expands every secret into its raw and URL-encoded forms, drops
// duplicates, and orders longest first so a longer secret is never broken apart
// by the replacement of a shorter one contained in it.
func buildTokens(secrets []string, primary, short map[string]bool) []redactToken {
	seen := make(map[string]bool)
	var tokens []redactToken
	appendToken := func(value string, isPrimary, isShort bool) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		tokens = append(tokens, redactToken{value: value, primary: isPrimary, short: isShort})
	}
	for _, secret := range secrets {
		appendToken(secret, primary[secret], short[secret])
		if escaped := url.QueryEscape(secret); escaped != secret {
			appendToken(escaped, primary[secret], short[secret])
			// Some encoders spell a space as %20 rather than the plus sign
			// QueryEscape emits; both spellings are the same material.
			appendToken(strings.ReplaceAll(escaped, "+", "%20"), primary[secret], short[secret])
		}
	}
	sort.Slice(tokens, func(i, j int) bool {
		if len(tokens[i].value) != len(tokens[j].value) {
			return len(tokens[i].value) > len(tokens[j].value)
		}
		return tokens[i].value < tokens[j].value
	})
	return tokens
}

// JSON redacts one provider response. The decoded document is walked once:
// values under known credential keys are stripped to the marker, URL and
// nested-JSON strings are cleaned up, and then every remaining string —
// including map keys — is passed through the single-pass exact sanitizer for
// configured API keys, secret header values and their URL-encoded forms. The
// re-encoded document is finally validated: if any known material can still be
// read out of it, in any position, the whole response is withheld with a
// generic error instead of a partially sanitized document.
func (r Redactor) JSON(body []byte) ([]byte, error) {
	if r.withheld {
		return nil, ErrRedactionWithheld
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	cleaned, err := r.sanitize(value, false)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return nil, ErrRedactionWithheld
	}
	if !r.noKnownMaterial(encoded) {
		return nil, ErrRedactionWithheld
	}
	return encoded, nil
}

func (r Redactor) sanitize(value any, credentialContext bool) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			processed, err := r.sanitize(child, credentialContext || sensitiveKey[strings.ToLower(k)])
			if err != nil {
				return nil, err
			}
			if sensitiveKey[strings.ToLower(k)] && child != nil && child != "" {
				processed = marker
			}
			key, err := r.redactKey(k)
			if err != nil {
				return nil, err
			}
			if _, exists := out[key]; exists {
				// Two distinct keys collapsed onto one rewritten key; emitting
				// the map would silently drop one of them.
				return nil, ErrRedactionWithheld
			}
			out[key] = processed
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			processed, err := r.sanitize(child, credentialContext)
			if err != nil {
				return nil, err
			}
			out[i] = processed
		}
		return out, nil
	case string:
		return r.sanitizeString(v, credentialContext)
	default:
		return value, nil
	}
}

// redactKey applies the exact sanitizer to a map key. Keys carry structure, so
// they count as credential context for short derived material.
func (r Redactor) redactKey(k string) (string, error) {
	return r.redactDecoded(k, true), nil
}

func (r Redactor) sanitizeString(s string, credentialContext bool) (string, error) {
	// Nested JSON is decoded and sanitized structurally first: a JSON document
	// is not a URL, and running URL cleanup over it would re-encode its quotes
	// and destroy the structure.
	if nested := strings.TrimSpace(s); (strings.HasPrefix(nested, "{") || strings.HasPrefix(nested, "[")) && json.Valid([]byte(nested)) {
		var child any
		if json.Unmarshal([]byte(nested), &child) == nil {
			cleaned, err := r.sanitize(child, credentialContext)
			if err != nil {
				return "", err
			}
			encoded, err := json.Marshal(cleaned)
			if err != nil {
				return "", ErrRedactionWithheld
			}
			s = string(encoded)
		}
	}
	if strings.Contains(s, "://") {
		if sanitized := r.URL(s); sanitized != s {
			s = sanitized
		}
	}
	return r.redactDecoded(s, credentialContext), nil
}

// redactDecoded matches against the original text and writes replacements to
// a separate buffer. Sorting tokens by length makes overlapping matches at
// the same offset deterministic.
func (r Redactor) redactDecoded(s string, credentialContext bool) string {
	if len(r.tokens) == 0 {
		return s
	}
	var out strings.Builder
	start := 0
	for i := 0; i < len(s); {
		matched := false
		for _, token := range r.tokens {
			if s[i] != token.value[0] || !strings.HasPrefix(s[i:], token.value) {
				continue
			}
			end := i + len(token.value)
			if !token.primary && ((token.short && !credentialContext) || !boundary(s, i, end)) {
				continue
			}
			out.WriteString(s[start:i])
			out.WriteString(marker)
			start, i = end, end
			matched = true
			break
		}
		if !matched {
			i++
		}
	}
	if start == 0 {
		return s
	}
	out.WriteString(s[start:])
	return out.String()
}

// replaceGuarded replaces only the occurrences of token whose position the
// caller accepts. Rejected occurrences are copied verbatim and the scan
// resumes after them; no occurrence is reconsidered and no recursion runs.
func replaceGuarded(s, token string, allow func(start, end int) bool) string {
	if token == "" {
		return s
	}
	var out strings.Builder
	from := 0
	changed := false
	for {
		rel := strings.Index(s[from:], token)
		if rel < 0 {
			break
		}
		i := from + rel
		end := i + len(token)
		if allow(i, end) {
			out.WriteString(s[from:i])
			out.WriteString(marker)
			from = end
			changed = true
		} else {
			out.WriteString(s[from : i+1])
			from = i + 1
		}
	}
	if !changed {
		return s
	}
	out.WriteString(s[from:])
	return out.String()
}

// noKnownMaterial validates the re-encoded document. Every raw and
// URL-encoded token must be gone, and so must the JSON-escaped spelling of a
// raw secret: characters the encoder escapes (quotes, control characters,
// HTML-significant characters) survive redaction only in that form.
func (r Redactor) noKnownMaterial(encoded []byte) bool {
	for _, t := range r.tokens {
		if bytes.Contains(encoded, []byte(t.value)) {
			return false
		}
	}
	for _, secret := range r.secrets {
		if escaped := jsonEscapedInner(secret); escaped != "" && escaped != secret && bytes.Contains(encoded, []byte(escaped)) {
			return false
		}
	}
	return true
}

// jsonEscapedInner returns the spelling a secret takes inside a JSON string
// after encoding: json.Marshal(secret) without its surrounding quotes.
func jsonEscapedInner(secret string) string {
	b, err := json.Marshal(secret)
	if err != nil || len(b) < 2 {
		return ""
	}
	return string(b[1 : len(b)-1])
}

// Diagnostic removes known material from provider error text. The fixed
// message replaces a diagnostic when exact cleanup cannot cover its encoding.
func (r Redactor) Diagnostic(value string) string {
	if len(r.tokens) == 0 {
		return value
	}
	cleaned := r.redactDecoded(value, true)
	if !r.noKnownMaterial([]byte(cleaned)) {
		return "provider returned a confidential diagnostic"
	}
	return cleaned
}

func (r Redactor) Text(body []byte) []byte {
	s := string(body)
	// URL sanitisation first handles credentials which contain punctuation.
	reURL := regexp.MustCompile(`https?://[^\s"'<>]+`)
	s = reURL.ReplaceAllStringFunc(s, r.URL)
	// These forms establish credential context, including for short keys.
	auth := regexp.MustCompile(`(?i)(\b(?:bearer|basic|token|apikey|api[-_]?key|authorization)\s*[:=]?\s+|\bapi[_-]?key\s*=\s*)([^\s,;&"']+)`)
	s = auth.ReplaceAllStringFunc(s, func(match string) string {
		loc := auth.FindStringSubmatchIndex(match)
		if len(loc) < 6 {
			return match
		}
		secret := match[loc[4]:loc[5]]
		return match[:loc[4]] + r.replaceText(secret, true) + match[loc[5]:]
	})
	return []byte(r.replaceText(s, false))
}

// replaceText keeps the text-path rules: every configured secret is candidate,
// but short derived material is only redacted inside established credential
// context and matches are bounded by identifier boundaries so ordinary words
// that merely contain a short secret survive.
func (r Redactor) replaceText(s string, credentialContext bool) string {
	for _, secret := range r.secrets {
		current := s
		s = replaceGuarded(current, secret, func(start, end int) bool {
			ok := boundary(current, start, end)
			if r.short[secret] && !credentialContext {
				ok = false
			}
			return ok
		})
	}
	return s
}

func (r Redactor) URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return r.replaceText(raw, true)
	}
	u.User = nil
	strip := func(rawQuery string) string {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return rawQuery
		}
		for k, values := range q {
			if sensitiveKey[strings.ToLower(k)] {
				nonempty := false
				for _, value := range values {
					if value != "" {
						nonempty = true
						break
					}
				}
				if nonempty {
					q.Del(k)
				}
			}
		}
		return q.Encode()
	}
	u.RawQuery = strip(u.RawQuery)
	u.Fragment = strip(u.Fragment)
	// Sanitising the URL structure does not catch a configured secret in an
	// ordinary parameter, nor its percent-encoded representation.
	sanitized := u.String()
	for _, secret := range r.secrets {
		if escaped := url.QueryEscape(secret); escaped != secret {
			sanitized = strings.ReplaceAll(sanitized, escaped, marker)
			sanitized = strings.ReplaceAll(sanitized, strings.ReplaceAll(escaped, "+", "%20"), marker)
		}
	}
	return sanitized
}

func identifier(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.'
}

func boundary(s string, start, end int) bool {
	if start > 0 {
		rr := []rune(s[:start])
		if len(rr) > 0 && identifier(rr[len(rr)-1]) {
			return false
		}
	}
	if end < len(s) {
		rr := []rune(s[end:])
		if len(rr) > 0 && identifier(rr[0]) {
			return false
		}
	}
	return true
}
