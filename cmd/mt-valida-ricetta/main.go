// Comando mt-valida-ricetta: valida una ricetta del catalogo PRIMA della firma (spec §1.9, ADR 0410 A4,
// lotto L4). È ciò che la CI esegue su ogni recipes/**/*.json e ciò che scripts/firma-ricetta.sh chiama
// prima di chiedere la passphrase: una ricetta che non passa qui non si firma.
//
//	mt-valida-ricetta [-adesso ISTANTE] [-tetto-giorni N] FILE...
//
// Controlli, tutti riportati (non ci si ferma al primo):
//   - decodifica stretta nello stesso tipo che usa il connettore (campi sconosciuti rifiutati) e
//     catalog.Recipe.Validate: gli stessi vincoli di objects/recipe.json applicati sull'host (§1.18);
//   - forma canonica: i byte del file sono IDENTICI a quelli che produce catalog.Canonical (chiavi
//     ordinate, senza spazi, senza a capo finale), cioè a `jq -cjS .`: la firma copre i byte del file;
//   - percorso: in recipes/<recipeId>/<recipeVersion>.json nome e versione coincidono col contenuto;
//   - scadenza: expiresAt dopo adesso e non oltre il tetto (180 giorni, §1.9);
//   - mai http://, mai la parola «latest» in nessuna stringa: versioni fissate (§1.9);
//   - source coerente coi passi: npm → ref `pacchetto@versione` installato da un passo
//     `npm install -g <ref>` e nessun fetch; release → ref è l'URL https di un passo fetch e nessun
//     passo npm;
//   - adapter.kind hooks → ultimo passo `monsterterminal-connector hooks install -yes` (decisione 7 del
//     piano di Fase 3); adapter.id uguale ad agent.
//
// Codici di uscita: 0 tutte valide, 1 almeno un errore, 2 uso sbagliato.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/monsterterminal/connector/internal/catalog"
)

// MaxValidityDays è il tetto di validità di una ricetta dalla firma (§1.9).
const MaxValidityDays = 180

var (
	npmRefRE = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*@(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$`)
	latestRE = regexp.MustCompile(`(?i)(^|[^a-z])latest([^a-z]|$)`)
	pathRE   = regexp.MustCompile(`recipes/([a-z0-9][a-z0-9-]{0,63})/([1-9][0-9]*)\.json$`)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("mt-valida-ricetta", flag.ContinueOnError)
	fs.SetOutput(stderr)
	adesso := fs.String("adesso", "", "istante RFC 3339 da usare come «adesso» (solo prove)")
	tetto := fs.Int("tetto-giorni", MaxValidityDays, "giorni massimi di validità dalla firma")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "uso: mt-valida-ricetta [-adesso ISTANTE] [-tetto-giorni N] FILE...")
		return 2
	}
	clock := now
	if *adesso != "" {
		t, err := time.Parse(time.RFC3339Nano, *adesso)
		if err != nil {
			fmt.Fprintf(stderr, "-adesso %q: %v\n", *adesso, err)
			return 2
		}
		clock = func() time.Time { return t }
	}
	code := 0
	for _, path := range fs.Args() {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, err)
			code = 1
			continue
		}
		errs := Check(path, raw, clock(), *tetto)
		if len(errs) == 0 {
			fmt.Fprintf(stdout, "ok   %s\n", path)
			continue
		}
		code = 1
		for _, e := range errs {
			fmt.Fprintf(stderr, "ERR  %s: %v\n", path, e)
		}
	}
	return code
}

// Check applica tutti i controlli a una ricetta e restituisce ogni errore trovato. path serve per la
// coerenza con recipes/<id>/<ver>.json e può essere vuoto.
func Check(path string, raw []byte, now time.Time, maxDays int) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r catalog.Recipe
	if err := dec.Decode(&r); err != nil {
		return []error{fmt.Errorf("non è una ricetta 0.11.0: %v", err)}
	}
	if _, err := dec.Token(); err != io.EOF {
		return []error{errors.New("contenuto dopo l'oggetto JSON")}
	}
	if err := r.Validate(raw); err != nil {
		add("%v", err)
	}

	canon, err := catalog.Canonical(json.RawMessage(raw))
	if err != nil {
		add("forma canonica non calcolabile: %v", err)
	} else if !bytes.Equal(canon, raw) {
		add("non è in forma canonica (chiavi ordinate, senza spazi, senza a capo finale): rigenerare con `jq -cjS . FILE`")
	}

	if m := pathRE.FindStringSubmatch(filepath.ToSlash(path)); m != nil {
		if m[1] != r.RecipeID {
			add("il percorso dice recipeId %q, il contenuto %q", m[1], r.RecipeID)
		}
		if v, _ := strconv.Atoi(m[2]); v != r.RecipeVersion {
			add("il percorso dice recipeVersion %s, il contenuto %d", m[2], r.RecipeVersion)
		}
	}

	if exp, err := time.Parse(time.RFC3339Nano, r.ExpiresAt); err == nil {
		switch {
		case !now.Before(exp):
			add("expiresAt %s è già passato", r.ExpiresAt)
		case exp.After(now.Add(time.Duration(maxDays) * 24 * time.Hour)):
			add("expiresAt %s è oltre %d giorni da adesso (%s)", r.ExpiresAt, maxDays, now.UTC().Format(time.RFC3339))
		}
	}

	if bytes.Contains(bytes.ToLower(raw), []byte("http://")) {
		add("contiene http://: solo https")
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err == nil {
		walkStrings(generic, "$", func(at, s string) {
			if latestRE.MatchString(s) {
				add("%s contiene «latest»: le versioni sono fissate", at)
			}
		})
	}

	errs = append(errs, checkSource(r)...)

	if r.Adapter.Kind == "hooks" {
		last := r.Steps[max(len(r.Steps)-1, 0)]
		want := []string{"/usr/local/bin/monsterterminal-connector", "hooks", "install", "-yes"}
		if len(r.Steps) == 0 || !equalArgs(last.Run, want) {
			add("adapter.kind hooks: l'ultimo passo deve essere run %q", strings.Join(want, " "))
		}
	}
	if r.Adapter.ID != "" && r.Agent != "" && r.Adapter.ID != r.Agent {
		add("adapter.id %q diverso da agent %q", r.Adapter.ID, r.Agent)
	}
	return errs
}

// checkSource verifica che source dica la verità sui passi.
func checkSource(r catalog.Recipe) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	var fetches []string
	npmInstalls := 0
	installsRef := false
	for _, s := range r.Steps {
		if s.Fetch != nil {
			fetches = append(fetches, s.Fetch.URL)
			continue
		}
		if len(s.Run) > 0 && filepath.Base(s.Run[0]) == "npm" {
			npmInstalls++
			if contains(s.Run, "install") && contains(s.Run, "-g") && contains(s.Run, r.Source.Ref) {
				installsRef = true
			}
		}
	}
	switch r.Source.Channel {
	case "npm":
		if !npmRefRE.MatchString(r.Source.Ref) {
			add("source npm: ref %q non è pacchetto@versione", r.Source.Ref)
		}
		if !installsRef {
			add("source npm: nessun passo `npm install -g %s`", r.Source.Ref)
		}
		if len(fetches) > 0 {
			add("source npm: una ricetta npm non ha passi fetch (%d trovati)", len(fetches))
		}
	case "release":
		if !strings.HasPrefix(r.Source.Ref, "https://") {
			add("source release: ref %q non è un URL https", r.Source.Ref)
		}
		if !contains(fetches, r.Source.Ref) {
			add("source release: nessun passo fetch scarica %s", r.Source.Ref)
		}
		if npmInstalls > 0 {
			add("source release: una ricetta release non chiama npm (%d passi)", npmInstalls)
		}
	}
	return errs
}

func walkStrings(v any, at string, fn func(at, s string)) {
	switch x := v.(type) {
	case string:
		fn(at, x)
	case []any:
		for i, e := range x {
			walkStrings(e, fmt.Sprintf("%s[%d]", at, i), fn)
		}
	case map[string]any:
		for k, e := range x {
			walkStrings(e, at+"."+k, fn)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
