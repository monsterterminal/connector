package catalog

// Validatore minimo di JSON Schema per i test di corrispondenza (#26 asse B): copre le parole chiave
// che gli schemi 0.11.0 usano ($ref, type, const, enum, properties, required, additionalProperties,
// items, minItems, maxItems, uniqueItems, minLength, maxLength, pattern, minimum, maximum, oneOf).
// Non ha dipendenze: il modulo del connettore non ne aggiunge per i test.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/monsterterminal/connector/internal/protocol"
)

func schemaRoot() string {
	return filepath.Join("..", "..", "schemas", protocol.SchemaVersion)
}

type validator struct {
	t     *testing.T
	cache map[string]map[string]any
}

func newValidator(t *testing.T) *validator {
	return &validator{t: t, cache: map[string]map[string]any{}}
}

func (v *validator) load(rel string) map[string]any {
	v.t.Helper()
	if s, ok := v.cache[rel]; ok {
		return s
	}
	raw, err := os.ReadFile(filepath.Join(schemaRoot(), rel))
	if err != nil {
		v.t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		v.t.Fatal(err)
	}
	v.cache[rel] = s
	return s
}

// validate dà nil se doc (già decodificato in any) rispetta lo schema rel.
func (v *validator) validate(rel string, doc any) error {
	return v.check(rel, v.load(rel), doc, "$")
}

// validateJSON decodifica raw e lo valida.
func (v *validator) validateJSON(rel string, raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	return v.validate(rel, doc)
}

func (v *validator) resolve(base, ref string) (string, map[string]any) {
	file, frag, _ := strings.Cut(ref, "#")
	rel := base
	if file != "" {
		rel = path.Clean(path.Join(path.Dir(base), file))
	}
	s := v.load(rel)
	if frag != "" {
		for _, p := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
			s = s[p].(map[string]any)
		}
	}
	return rel, s
}

func (v *validator) check(base string, s map[string]any, doc any, at string) error {
	if ref, ok := s["$ref"].(string); ok {
		rel, rs := v.resolve(base, ref)
		if err := v.check(rel, rs, doc, at); err != nil {
			return err
		}
	}
	if c, ok := s["const"]; ok && fmt.Sprint(c) != fmt.Sprint(doc) {
		return fmt.Errorf("%s: %v diverso da const %v", at, doc, c)
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if fmt.Sprint(x) == fmt.Sprint(doc) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: %v fuori enum %v", at, doc, e)
		}
	}
	if one, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, o := range one {
			if v.check(base, o.(map[string]any), doc, at) == nil {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%s: oneOf soddisfatto da %d rami", at, n)
		}
	}
	if typ, ok := s["type"].(string); ok {
		if err := checkType(typ, doc); err != nil {
			return fmt.Errorf("%s: %v", at, err)
		}
	}
	switch d := doc.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range asList(s["required"]) {
			if _, ok := d[r]; !ok {
				return fmt.Errorf("%s: manca %q", at, r)
			}
		}
		for k, val := range d {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if ap, set := s["additionalProperties"].(bool); set && !ap {
					return fmt.Errorf("%s: campo sconosciuto %q", at, k)
				}
				continue
			}
			if err := v.check(base, ps, val, at+"."+k); err != nil {
				return err
			}
		}
	case []any:
		if m, ok := s["minItems"].(float64); ok && float64(len(d)) < m {
			return fmt.Errorf("%s: %d elementi, minimo %v", at, len(d), m)
		}
		if m, ok := s["maxItems"].(float64); ok && float64(len(d)) > m {
			return fmt.Errorf("%s: %d elementi, massimo %v", at, len(d), m)
		}
		if u, _ := s["uniqueItems"].(bool); u {
			seen := map[string]bool{}
			for _, x := range d {
				k := fmt.Sprint(x)
				if seen[k] {
					return fmt.Errorf("%s: elemento ripetuto %v", at, x)
				}
				seen[k] = true
			}
		}
		if is, ok := s["items"].(map[string]any); ok {
			for i, x := range d {
				if err := v.check(base, is, x, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	case string:
		n := utf8.RuneCountInString(d)
		if m, ok := s["minLength"].(float64); ok && float64(n) < m {
			return fmt.Errorf("%s: lunghezza %d sotto %v", at, n, m)
		}
		if m, ok := s["maxLength"].(float64); ok && float64(n) > m {
			return fmt.Errorf("%s: lunghezza %d oltre %v", at, n, m)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(d) {
			return fmt.Errorf("%s: %q non rispetta %s", at, d, p)
		}
	case float64:
		if m, ok := s["minimum"].(float64); ok && d < m {
			return fmt.Errorf("%s: %v sotto %v", at, d, m)
		}
		if m, ok := s["maximum"].(float64); ok && d > m {
			return fmt.Errorf("%s: %v oltre %v", at, d, m)
		}
	}
	return nil
}

func checkType(typ string, doc any) error {
	ok := false
	switch typ {
	case "object":
		_, ok = doc.(map[string]any)
	case "array":
		_, ok = doc.([]any)
	case "string":
		_, ok = doc.(string)
	case "boolean":
		_, ok = doc.(bool)
	case "integer":
		f, isNum := doc.(float64)
		ok = isNum && f == float64(int64(f))
	case "number":
		_, ok = doc.(float64)
	}
	if !ok {
		return fmt.Errorf("%v non è %s", doc, typ)
	}
	return nil
}

func asList(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

// TestValidatoreSiAccorgeDegliErrori: il validatore dei test rifiuta davvero (un validatore che dice
// sempre sì renderebbe inutili i test di corrispondenza).
func TestValidatoreSiAccorgeDegliErrori(t *testing.T) {
	v := newValidator(t)
	ok := `{"installId":"6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b","outcome":"failed","reason":"timeout","stepIndex":0}`
	env := func(body string) []byte {
		return []byte(`{"type":"catalog.install.result","id":"6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b","ts":"2026-11-26T10:00:00.000Z","body":` + body + `}`)
	}
	rel := "messages/connector-to-backend/catalog.install.result.json"
	if err := v.validateJSON(rel, env(ok)); err != nil {
		t.Fatalf("esito valido rifiutato: %v", err)
	}
	for name, body := range map[string]string{
		"motivo fuori enum":   strings.Replace(ok, `"timeout"`, `"boh"`, 1),
		"campo sconosciuto":   strings.Replace(ok, `"stepIndex":0`, `"stepIndex":0,"x":1`, 1),
		"stepIndex oltre 31":  strings.Replace(ok, `"stepIndex":0`, `"stepIndex":32`, 1),
		"installId non uuid":  strings.Replace(ok, `6f1c2a7e`, `zz`, 1),
		"outcome mancante":    strings.Replace(ok, `"outcome":"failed",`, ``, 1),
		"stepIndex non int":   strings.Replace(ok, `"stepIndex":0`, `"stepIndex":0.5`, 1),
		"logTail oltre 8 KiB": strings.Replace(ok, `"stepIndex":0`, `"logTail":"`+strings.Repeat("a", 8193)+`"`, 1),
	} {
		if err := v.validateJSON(rel, env(body)); err == nil {
			t.Errorf("%s: accettato", name)
		}
	}
}
