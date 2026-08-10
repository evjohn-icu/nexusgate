// Package secretredact removes provider credentials from data returned across
// a boundary. It is intentionally independent of the credential and HTTP
// packages so the same rules can be used by logs and proxy responses.
package secretredact

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const marker = "[REDACTED]"

type Material struct {
	APIKey       string
	ExtraHeaders map[string]string
	BaseURL      string
}

type Redactor struct {
	secrets []string
	short   map[string]bool
}

var sensitiveKey = map[string]bool{
	"authorization": true, "api_key": true, "apikey": true, "token": true,
	"secret": true, "password": true, "access_token": true, "credential": true,
	"key": true,
}

func New(m Material) Redactor {
	r := Redactor{short: make(map[string]bool)}
	seen := make(map[string]bool)
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		r.secrets = append(r.secrets, s)
		r.short[s] = len([]byte(s)) < 8
	}
	add(m.APIKey)
	for name, s := range m.ExtraHeaders {
		lower := strings.ToLower(name)
		if sensitiveKey[lower] || strings.Contains(lower, "auth") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "key") {
			add(s)
		}
	}
	if u, err := url.Parse(m.BaseURL); err == nil {
		if u.User == nil {
			// Query and fragment credentials are still material even without userinfo.
		}
		if u.User != nil {
			if p, ok := u.User.Password(); ok && p != "" {
				add(p)
			}
			add(u.User.Username())
		}
		for _, values := range u.Query() {
			for _, value := range values {
				add(value)
			}
		}
		if fragment, err := url.ParseQuery(u.Fragment); err == nil {
			for _, values := range fragment {
				for _, value := range values {
					add(value)
				}
			}
		}
	}
	return r
}

func (r Redactor) JSON(body []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	r.jsonValue(&value, false)
	return json.Marshal(value)
}

func (r Redactor) jsonValue(value *any, credentialContext bool) {
	switch v := (*value).(type) {
	case map[string]any:
		for k, child := range v {
			ctx := credentialContext || sensitiveKey[strings.ToLower(k)]
			if sensitiveKey[strings.ToLower(k)] && child != nil && child != "" {
				v[k] = marker
				continue
			}
			r.jsonValue(&child, ctx)
			v[k] = child
		}
	case []any:
		for i := range v {
			r.jsonValue(&v[i], credentialContext)
		}
	case string:
		if strings.Contains(v, "://") {
			if sanitized := r.URL(v); sanitized != v {
				*value = sanitized
				return
			}
		}
		if nested := strings.TrimSpace(v); (strings.HasPrefix(nested, "{") || strings.HasPrefix(nested, "[")) && json.Valid([]byte(nested)) {
			var child any
			if json.Unmarshal([]byte(nested), &child) == nil {
				r.jsonValue(&child, credentialContext)
				b, _ := json.Marshal(child)
				*value = string(b)
				return
			}
		}
		*value = r.replace(v, credentialContext)
	}
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
		return match[:loc[4]] + r.replace(secret, true) + match[loc[5]:]
	})
	return []byte(r.replace(s, false))
}

func (r Redactor) URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return r.replace(raw, true)
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
	return u.String()
}

func identifier(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

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

func (r Redactor) replace(s string, credentialContext bool) string {
	for _, secret := range r.secrets {
		if secret == "" {
			continue
		}
		for {
			i := strings.Index(s, secret)
			if i < 0 {
				break
			}
			ok := boundary(s, i, i+len(secret))
			if r.short[secret] && !credentialContext {
				// Short values are only safe when the JSON walk or text parser has
				// already established credential context.
				ok = false
			}
			if !ok {
				s = s[:i+len(secret)] + r.replace(s[i+len(secret):], credentialContext)
				break
			}
			s = s[:i] + marker + s[i+len(secret):]
		}
	}
	return s
}
