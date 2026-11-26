package catalog

// Test della ricetta e delle verifiche (spec §1.9, §1.18, ADR 0410 A4). Asse A: ParseRecipe e Check,
// ogni motivo con un caso che lo produce e uno che non lo produce, nell'ordine delle verifiche.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/signing"
)

type keyPair struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKey(t *testing.T) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{pub, priv}
}

func (k keyPair) sign(raw []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, raw))
}

// testRecipe è una ricetta conforme allo schema 0.11.0 con un passo fetch e uno run.
func testRecipe() Recipe {
	return Recipe{
		RecipeID: "opencode", RecipeVersion: 2, ExpiresAt: "2027-05-25T00:00:00Z",
		Source:    Source{Channel: "release", Ref: "https://example.com/v2.3.0/opencode.tar.gz"},
		Agent:     "opencode",
		Platforms: []Platform{{OS: "linux", Arch: []string{"amd64"}, Distro: []string{"debian", "ubuntu"}}},
		Requires:  []Require{},
		Steps: []Step{
			{Fetch: &Fetch{URL: "https://example.com/v2.3.0/opencode.tar.gz", SHA256: strings.Repeat("ab", 32), Dest: "opencode.tar.gz"}, TimeoutSeconds: 300},
			{Run: []string{"/usr/local/bin/monsterterminal-connector", "catalog", "extract", "{work}/opencode.tar.gz", ".local/opencode"}, TimeoutSeconds: 60},
		},
		Auth:    Auth{Instructions: "Open a session and run: opencode auth login"},
		Start:   []string{"opencode"},
		Adapter: Adapter{Kind: "generic", ID: "opencode"},
	}
}

func canon(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// canonMap ritocca la ricetta come mappa (campi in più o valori fuori schema) e la porta in forma canonica.
func canonMap(t *testing.T, r Recipe, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(canon(t, r), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	return canon(t, m)
}

func TestCanonicalOrdinaLeChiaviSenzaSpazi(t *testing.T) {
	got := string(canon(t, map[string]any{"b": 1, "a": map[string]any{"d": "<&>", "c": []int{1}}}))
	if got != `{"a":{"c":[1],"d":"<&>"},"b":1}` {
		t.Fatalf("forma canonica: %s", got)
	}
	raw := canon(t, testRecipe())
	if bytes.Contains(raw, []byte("\n")) || bytes.Contains(raw, []byte(`": `)) || bytes.Contains(raw, []byte(`, "`)) {
		t.Fatalf("spazi fuori dalle stringhe: %s", raw)
	}
	if !bytes.HasPrefix(raw, []byte(`{"adapter":`)) {
		t.Fatalf("chiavi non ordinate: %s", raw)
	}
}

func TestParseRecipeFirmaValida(t *testing.T) {
	k := newKey(t)
	raw := canon(t, testRecipe())
	r, err := ParseRecipe(raw, k.sign(raw), k.pub)
	if err != nil {
		t.Fatalf("ricetta firmata rifiutata: %v", err)
	}
	if r.RecipeID != "opencode" || len(r.Steps) != 2 || r.Steps[0].Fetch == nil || r.Steps[1].Run[0] != "/usr/local/bin/monsterterminal-connector" {
		t.Fatalf("ricetta letta male: %+v", r)
	}
}

// §1.17-§1.18: la ricetta si verifica con la chiave delle RICETTE; una firma di un'altra chiave (quella
// degli aggiornamenti compresa) non passa.
func TestParseRecipeChiaveDegliAggiornamentiRifiutata(t *testing.T) {
	updates, recipes := newKey(t), newKey(t)
	raw := canon(t, testRecipe())
	if _, err := ParseRecipe(raw, updates.sign(raw), recipes.pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("firma della chiave sbagliata: %v", err)
	}
	// La chiave di default è mt-recipes-2026, mai mt-updates-2026.
	rk, _ := signing.PublicKey(signing.Recipes)
	uk, _ := signing.PublicKey(signing.Updates)
	if !bytes.Equal(RecipesKey(), rk) || bytes.Equal(RecipesKey(), uk) {
		t.Fatal("la chiave di default delle ricette non è mt-recipes-2026")
	}
	// Con la chiave di default una ricetta firmata da una chiave qualunque non passa.
	if _, err := ParseRecipe(raw, recipes.sign(raw), nil); !errors.Is(err, ErrSignature) {
		t.Fatalf("firma usa e getta accettata dalla chiave incorporata: %v", err)
	}
}

func TestParseRecipeByteAlterati(t *testing.T) {
	k := newKey(t)
	raw := canon(t, testRecipe())
	sig := k.sign(raw)
	altered := bytes.Replace(raw, []byte(`"recipeVersion":2`), []byte(`"recipeVersion":3`), 1)
	if _, err := ParseRecipe(altered, sig, k.pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("byte alterati: %v", err)
	}
	// Anche una riserializzazione con gli spazi (stesso contenuto, altri byte) non passa: si
	// verificano i byte ricevuti, non il significato.
	spaced := bytes.Replace(raw, []byte(`,`), []byte(`, `), 1)
	if _, err := ParseRecipe(spaced, sig, k.pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("byte riserializzati: %v", err)
	}
	for name, s := range map[string]string{"vuota": "", "non base64": "@@@", "corta": base64.StdEncoding.EncodeToString([]byte("x"))} {
		if _, err := ParseRecipe(raw, s, k.pub); !errors.Is(err, ErrSignature) {
			t.Errorf("firma %s: %v", name, err)
		}
	}
}

func TestParseRecipeCampoSconosciutoEFuoriSchema(t *testing.T) {
	k := newKey(t)
	cases := map[string]func(m map[string]any){
		"campo sconosciuto":   func(m map[string]any) { m["extra"] = 1 },
		"campo nel passo":     func(m map[string]any) { m["steps"].([]any)[1].(map[string]any)["shell"] = true },
		"run e fetch insieme": func(m map[string]any) { m["steps"].([]any)[0].(map[string]any)["run"] = []any{"x"} },
		"url http": func(m map[string]any) {
			m["steps"].([]any)[0].(map[string]any)["fetch"].(map[string]any)["url"] = "http://example.com/x"
		},
		"dest con barra": func(m map[string]any) {
			m["steps"].([]any)[0].(map[string]any)["fetch"].(map[string]any)["dest"] = "../x"
		},
		"dest nascosto": func(m map[string]any) {
			m["steps"].([]any)[0].(map[string]any)["fetch"].(map[string]any)["dest"] = ".bashrc"
		},
		"sha256 maiuscolo": func(m map[string]any) {
			m["steps"].([]any)[0].(map[string]any)["fetch"].(map[string]any)["sha256"] = strings.Repeat("AB", 32)
		},
		"senza expiresAt":    func(m map[string]any) { delete(m, "expiresAt") },
		"senza source":       func(m map[string]any) { delete(m, "source") },
		"senza requires":     func(m map[string]any) { delete(m, "requires") },
		"senza istruzioni":   func(m map[string]any) { m["auth"] = map[string]any{} },
		"canale sconosciuto": func(m map[string]any) { m["source"].(map[string]any)["channel"] = "curl" },
		"adapter sconosciuto": func(m map[string]any) {
			m["adapter"].(map[string]any)["kind"] = "shell"
		},
		"timeout oltre un'ora": func(m map[string]any) { m["steps"].([]any)[1].(map[string]any)["timeoutSeconds"] = 3601 },
		"run vuoto":            func(m map[string]any) { m["steps"].([]any)[1].(map[string]any)["run"] = []any{} },
		"trentatré passi": func(m map[string]any) {
			s := m["steps"].([]any)
			for len(s) < 33 {
				s = append(s, s[1])
			}
			m["steps"] = s
		},
		"distribuzione sconosciuta": func(m map[string]any) {
			m["platforms"].([]any)[0].(map[string]any)["distro"] = []any{"arch"}
		},
		"minVersion non semver": func(m map[string]any) {
			m["requires"] = []any{map[string]any{"check": "command", "name": "node", "minVersion": "22"}}
		},
	}
	v := newValidator(t)
	for name, edit := range cases {
		raw := canonMap(t, testRecipe(), edit)
		// La firma è valida: è il contenuto a non rispettare lo schema.
		if _, err := ParseRecipe(raw, k.sign(raw), k.pub); !errors.Is(err, ErrRecipe) {
			t.Errorf("%s: atteso ErrRecipe, ottenuto %v", name, err)
		}
		// Asse B: lo schema 0.11.0 rifiuta la stessa ricetta.
		if err := v.validateJSON("objects/recipe.json", raw); err == nil {
			t.Errorf("%s: lo schema la accetta, il connettore no", name)
		}
	}
	// E la ricetta di base la accettano entrambi.
	if err := v.validateJSON("objects/recipe.json", canon(t, testRecipe())); err != nil {
		t.Fatalf("ricetta di base fuori schema: %v", err)
	}
}

// goodEnv è un host che passa tutte le verifiche.
func goodEnv(k keyPair) Env {
	return Env{
		CatalogInstall: true, Key: k.pub,
		Now:      func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) },
		Platform: HostPlatform{OS: "linux", Arch: "amd64", Distro: "ubuntu"},
		LookupVersion: func(name string) (string, bool) {
			if name == "node" {
				return "22.3.0", true
			}
			return "", false
		},
		Installed: func(string) (int, bool, error) { return 1, true, nil },
		Acquire:   func() bool { return true },
	}
}

func withNode(r Recipe, min string) Recipe {
	r.Requires = []Require{{Check: "command", Name: "node", MinVersion: min}}
	return r
}

// Ogni motivo di rifiuto con il caso che lo produce e quello che non lo produce (§1.18).
func TestCheckOgniMotivo(t *testing.T) {
	k := newKey(t)
	base := withNode(testRecipe(), "22.0.0")
	type tc struct {
		reason string
		env    func(e *Env)
		recipe func(r *Recipe)
		sig    func(raw []byte) string
	}
	cases := []tc{
		{reason: protocol.ReasonDisabledOnHost, env: func(e *Env) { e.CatalogInstall = false }},
		{reason: protocol.ReasonSignatureInvalid, sig: func(raw []byte) string { return newKey(t).sign(raw) }},
		{reason: protocol.ReasonExpired, recipe: func(r *Recipe) { r.ExpiresAt = "2026-11-26T12:00:00Z" }},
		{reason: protocol.ReasonPlatformMismatch, env: func(e *Env) { e.Platform.Arch = "arm64" }},
		{reason: protocol.ReasonPlatformMismatch, env: func(e *Env) { e.Platform.Distro = "rhel" }},
		{reason: protocol.ReasonRootNotAllowed, env: func(e *Env) { e.Root = true }},
		{reason: protocol.ReasonRequiresFailed, env: func(e *Env) { e.LookupVersion = func(string) (string, bool) { return "", false } }},
		{reason: protocol.ReasonRequiresFailed, env: func(e *Env) { e.LookupVersion = func(string) (string, bool) { return "18.19.0", true } }},
		{reason: protocol.ReasonDowngrade, env: func(e *Env) { e.Installed = func(string) (int, bool, error) { return 2, true, nil } }},
		{reason: protocol.ReasonDowngrade, env: func(e *Env) { e.Installed = func(string) (int, bool, error) { return 5, true, nil } }},
		{reason: protocol.ReasonBusy, env: func(e *Env) { e.Acquire = func() bool { return false } }},
	}
	sign := func(r Recipe, sigf func([]byte) string) ([]byte, string) {
		raw := canon(t, r)
		if sigf != nil {
			return raw, sigf(raw)
		}
		return raw, k.sign(raw)
	}
	// Caso che NON lo produce: l'host buono e la ricetta buona passano.
	raw, sig := sign(base, nil)
	if _, ref := Check(goodEnv(k), raw, sig); ref != nil {
		t.Fatalf("host e ricetta buoni rifiutati: %v", ref)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		e := goodEnv(k)
		if c.env != nil {
			c.env(&e)
		}
		r := base
		if c.recipe != nil {
			c.recipe(&r)
		}
		raw, sig := sign(r, c.sig)
		_, ref := Check(e, raw, sig)
		if ref == nil || ref.Reason != c.reason {
			t.Errorf("atteso %s, ottenuto %v", c.reason, ref)
			continue
		}
		if len([]rune(ref.Detail)) > protocol.MaxCatalogDetail {
			t.Errorf("%s: detail oltre il tetto", c.reason)
		}
		seen[c.reason] = true
	}
	for _, r := range protocol.CatalogReasons {
		if !IsFailedReason(r) && !seen[r] {
			t.Errorf("motivo di rifiuto %s senza un caso che lo produce", r)
		}
	}
}

// Il primo rifiuto vince: con tutto sbagliato, l'ordine di §1.9 decide il motivo.
func TestCheckOrdineDelleVerifiche(t *testing.T) {
	k := newKey(t)
	r := withNode(testRecipe(), "22.0.0")
	r.ExpiresAt = "2020-01-01T00:00:00Z"
	raw := canon(t, r)
	acquired := false
	e := Env{
		CatalogInstall: false, Key: k.pub, Now: time.Now, Platform: HostPlatform{OS: "linux", Arch: "arm64", Distro: "rhel"}, Root: true,
		LookupVersion: func(string) (string, bool) { return "", false },
		Installed:     func(string) (int, bool, error) { return 9, true, nil },
		Acquire:       func() bool { acquired = true; return false },
	}
	order := []string{protocol.ReasonDisabledOnHost, protocol.ReasonSignatureInvalid, protocol.ReasonExpired, protocol.ReasonPlatformMismatch,
		protocol.ReasonRootNotAllowed, protocol.ReasonRequiresFailed, protocol.ReasonDowngrade, protocol.ReasonBusy}
	fix := []func(){
		func() { e.CatalogInstall = true },
		func() {}, // la firma si aggiusta firmando: sig sotto
		func() {
			r.ExpiresAt = "2099-01-01T00:00:00Z"
			raw = canon(t, r)
		},
		func() { e.Platform = HostPlatform{OS: "linux", Arch: "amd64", Distro: "debian"} },
		func() { e.Root = false },
		func() { e.LookupVersion = func(string) (string, bool) { return "22.0.0", true } },
		func() { e.Installed = func(string) (int, bool, error) { return 0, false, nil } },
	}
	sig := "AAAA" // firma malformata finché non si aggiusta
	for i, want := range order {
		s := sig
		if i > 1 {
			s = k.sign(raw)
		}
		_, ref := Check(e, raw, s)
		if ref == nil || ref.Reason != want {
			t.Fatalf("passo %d: atteso %s, ottenuto %v", i, want, ref)
		}
		if want != protocol.ReasonBusy && acquired {
			t.Fatalf("il turno è stato chiesto prima delle verifiche (%s)", want)
		}
		if i < len(fix) {
			fix[i]()
		}
	}
}

func TestCheckRequiresDettaglio(t *testing.T) {
	k := newKey(t)
	r := withNode(testRecipe(), "22.0.0")
	raw := canon(t, r)
	e := goodEnv(k)
	e.LookupVersion = func(string) (string, bool) { return "18.19.0", true }
	_, ref := Check(e, raw, k.sign(raw))
	if ref == nil || ref.Detail != "node >= 22.0.0, trovato 18.19.0" {
		t.Fatalf("detail di requires-failed: %+v", ref)
	}
	e.LookupVersion = func(string) (string, bool) { return "", false }
	if _, ref := Check(e, raw, k.sign(raw)); ref == nil || ref.Detail != "node >= 22.0.0, non trovato" {
		t.Fatalf("detail del requisito assente: %+v", ref)
	}
	e.LookupVersion = func(string) (string, bool) { return "", true }
	if _, ref := Check(e, raw, k.sign(raw)); ref == nil || !strings.Contains(ref.Detail, "illeggibile") {
		t.Fatalf("detail della versione illeggibile: %+v", ref)
	}
	// Senza minVersion basta che il comando ci sia; versione uguale alla minima passa.
	r.Requires = []Require{{Check: "command", Name: "npm"}}
	raw = canon(t, r)
	e.LookupVersion = func(string) (string, bool) { return "", true }
	if _, ref := Check(e, raw, k.sign(raw)); ref != nil {
		t.Fatalf("requisito senza versione presente: %v", ref)
	}
	r = withNode(testRecipe(), "22.0.0")
	raw = canon(t, r)
	e.LookupVersion = func(string) (string, bool) { return "22.0.0", true }
	if _, ref := Check(e, raw, k.sign(raw)); ref != nil {
		t.Fatalf("versione uguale alla minima: %v", ref)
	}
	e.LookupVersion = func(string) (string, bool) { return "22.0.0-rc.1", true }
	if _, ref := Check(e, raw, k.sign(raw)); ref == nil || ref.Reason != protocol.ReasonRequiresFailed {
		t.Fatalf("pre-release sotto la minima: %v", ref)
	}
}

// Il downgrade si legge dal file di stato state-dir/catalog/<recipeId>.json.
func TestCheckDowngradeDalFileDiStato(t *testing.T) {
	k := newKey(t)
	dir := t.TempDir()
	r := testRecipe()
	raw := canon(t, r)
	e := goodEnv(k)
	e.Installed = func(id string) (int, bool, error) {
		s, ok, err := ReadState(dir, id)
		return s.RecipeVersion, ok, err
	}
	if _, ref := Check(e, raw, k.sign(raw)); ref != nil {
		t.Fatalf("mai installata: %v", ref)
	}
	for _, v := range []int{2, 3} {
		if err := writeState(dir, State{RecipeID: "opencode", RecipeVersion: v, Agent: "opencode"}); err != nil {
			t.Fatal(err)
		}
		if _, ref := Check(e, raw, k.sign(raw)); ref == nil || ref.Reason != protocol.ReasonDowngrade {
			t.Fatalf("installata %d, ricetta 2: %v", v, ref)
		}
	}
	if err := writeState(dir, State{RecipeID: "opencode", RecipeVersion: 1, Agent: "opencode"}); err != nil {
		t.Fatal(err)
	}
	if _, ref := Check(e, raw, k.sign(raw)); ref != nil {
		t.Fatalf("installata 1, ricetta 2: %v", ref)
	}
	// Un file di stato rovinato non autorizza a reinstallare sopra.
	if err := os.WriteFile(StatePath(dir, "opencode"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ref := Check(e, raw, k.sign(raw)); ref == nil || ref.Reason != protocol.ReasonDowngrade {
		t.Fatalf("file di stato illeggibile: %v", ref)
	}
}
