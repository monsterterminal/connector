// Package catalog esegue le ricette firmate del catalogo (spec §1.9, §1.17, ADR 0410 D4 e A4, schemi
// 0.11.0): verifica nell'ordine di §1.9 (interruttore dell'host, firma, scadenza, piattaforma, root,
// requisiti, downgrade, occupato), poi esegue i passi uno alla volta con l'utente del connettore,
// senza shell, e riporta progresso ed esito. Nessun passo si esegue prima che tutte le verifiche
// siano passate; il connettore non espone nessun altro modo di eseguire un comando.
package catalog

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/signing"
)

// Recipe è la ricetta (objects/recipe.json, 0.11.0).
type Recipe struct {
	RecipeID      string     `json:"recipeId"`
	RecipeVersion int        `json:"recipeVersion"`
	ExpiresAt     string     `json:"expiresAt"`
	Source        Source     `json:"source"`
	Agent         string     `json:"agent"`
	Platforms     []Platform `json:"platforms"`
	Requires      []Require  `json:"requires"`
	Steps         []Step     `json:"steps"`
	Auth          Auth       `json:"auth"`
	Start         []string   `json:"start"`
	Adapter       Adapter    `json:"adapter"`
}

// Source è il canale ufficiale da cui viene l'agente.
type Source struct {
	Channel string `json:"channel"`
	Ref     string `json:"ref"`
}

// Platform è una piattaforma ammessa.
type Platform struct {
	OS     string   `json:"os"`
	Arch   []string `json:"arch"`
	Distro []string `json:"distro"`
}

// Require è un requisito: un comando presente, con la versione minima se indicata.
type Require struct {
	Check      string `json:"check"`
	Name       string `json:"name"`
	MinVersion string `json:"minVersion,omitempty"`
}

// Step è un passo: run OPPURE fetch, mai entrambi.
type Step struct {
	Run            []string `json:"run,omitempty"`
	Fetch          *Fetch   `json:"fetch,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}

// Fetch scarica un artefatto versionato nella cartella di lavoro.
type Fetch struct {
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Dest       string `json:"dest"`
	Executable bool   `json:"executable,omitempty"`
}

// Auth porta la guida breve, in inglese, all'autenticazione (la completa l'utente, ADR 0410 D4).
type Auth struct {
	Instructions string `json:"instructions"`
}

// Adapter dice come il connettore legge lo stato dell'agente.
type Adapter struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

var (
	recipeIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	agentIDRE  = recipeIDRE
	reqNameRE  = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// DestRE è fetch.dest dello schema: un nome di file, niente barre, niente punto iniziale.
	DestRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$`)
)

// Enum dello schema 0.11.0, nello stesso ordine.
var (
	SourceChannels = []string{"npm", "release"}
	AdapterKinds   = []string{"hooks", "output-patterns", "generic", "local-api"}
	OSes           = []string{"linux"}
	Arches         = []string{"amd64", "arm64"}
	Distros        = []string{"debian", "ubuntu", "rhel"}
	RequireChecks  = []string{"command"}
)

// Errori di ParseRecipe: entrambi diventano il rifiuto signature-invalid (§1.18: una ricetta con un
// campo sconosciuto o alterata di un byte è signature-invalid).
var (
	ErrSignature = errors.New("firma della ricetta non valida")
	ErrRecipe    = errors.New("ricetta non conforme allo schema 0.11.0")
)

// RecipesKey è la chiave pubblica delle ricette incorporata nel binario (mt-recipes-2026), mai quella
// degli aggiornamenti (§1.17).
func RecipesKey() ed25519.PublicKey {
	k, err := signing.PublicKey(signing.Recipes)
	if err != nil {
		panic("catalog: chiave delle ricette assente: " + err.Error())
	}
	return k
}

// ParseRecipe verifica la firma Ed25519 di key sui byte RICEVUTI della ricetta, tal quali (la forma
// canonica è quella che viaggia: si verifica ciò che è arrivato, mai una riserializzazione), e solo
// dopo la legge, rifiutando campi sconosciuti e valori fuori dallo schema. key nil = RecipesKey().
// signature è la firma in base64 (common.json#signature).
func ParseRecipe(raw []byte, signature string, key ed25519.PublicKey) (Recipe, error) {
	if key == nil {
		key = RecipesKey()
	}
	sig, err := protocol.DecodeSignature(signature)
	if err != nil {
		return Recipe{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	if !ed25519.Verify(key, raw, sig) {
		return Recipe{}, ErrSignature
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Recipe
	if err := dec.Decode(&r); err != nil {
		return Recipe{}, fmt.Errorf("%w: %v", ErrRecipe, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Recipe{}, fmt.Errorf("%w: contenuto dopo l'oggetto", ErrRecipe)
	}
	if err := r.Validate(raw); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRecipe, fmt.Sprintf(format, a...))
}

func in(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func uniqueIn(vs, set []string) bool {
	seen := map[string]bool{}
	for _, v := range vs {
		if seen[v] || !in(v, set) {
			return false
		}
		seen[v] = true
	}
	return len(vs) > 0
}

// Validate applica i vincoli di objects/recipe.json che la decodifica non controlla. raw sono i byte
// della ricetta, per i campi obbligatori che il tipo Go non distingue dallo zero (requires, run/fetch).
func (r Recipe) Validate(raw []byte) error {
	// auth.instructions è obbligatorio ma può essere vuoto: il tipo Go non distingue assente da "".
	var keys struct {
		Auth struct {
			Instructions *string `json:"instructions"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &keys); err != nil {
		return bad("%v", err)
	}
	switch {
	case !recipeIDRE.MatchString(r.RecipeID):
		return bad("recipeId %q", r.RecipeID)
	case r.RecipeVersion < 1:
		return bad("recipeVersion %d", r.RecipeVersion)
	case !agentIDRE.MatchString(r.Agent):
		return bad("agent %q", r.Agent)
	case !in(r.Source.Channel, SourceChannels) || r.Source.Ref == "" || len([]rune(r.Source.Ref)) > 512:
		return bad("source")
	case len(r.Platforms) == 0:
		return bad("platforms vuoto")
	case r.Requires == nil: // [] decodifica in una lista vuota, assente o null in nil
		return bad("requires mancante")
	case len(r.Steps) == 0 || len(r.Steps) > protocol.MaxCatalogSteps:
		return bad("steps: %d passi, ammessi da 1 a %d", len(r.Steps), protocol.MaxCatalogSteps)
	case keys.Auth.Instructions == nil || len([]rune(r.Auth.Instructions)) > 2000:
		return bad("auth.instructions")
	case len(r.Start) == 0:
		return bad("start vuoto")
	case !in(r.Adapter.Kind, AdapterKinds) || !agentIDRE.MatchString(r.Adapter.ID):
		return bad("adapter")
	}
	if !strings.HasSuffix(r.ExpiresAt, "Z") {
		return bad("expiresAt non è un istante UTC")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.ExpiresAt); err != nil {
		return bad("expiresAt: %v", err)
	}
	for _, p := range r.Platforms {
		if !in(p.OS, OSes) || !uniqueIn(p.Arch, Arches) || !uniqueIn(p.Distro, Distros) {
			return bad("platforms: %+v", p)
		}
	}
	for _, q := range r.Requires {
		if !in(q.Check, RequireChecks) || !reqNameRE.MatchString(q.Name) || (q.MinVersion != "" && !semverRE.MatchString(q.MinVersion)) {
			return bad("requires: %+v", q)
		}
	}
	for _, s := range r.Start {
		if s == "" || len([]rune(s)) > 4096 {
			return bad("start")
		}
	}
	for i, s := range r.Steps {
		if err := s.validate(); err != nil {
			return bad("steps[%d]: %v", i, err)
		}
	}
	return nil
}

func (s Step) validate() error {
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 3600 {
		return fmt.Errorf("timeoutSeconds %d fuori da 1-3600", s.TimeoutSeconds)
	}
	switch {
	case s.Fetch != nil && s.Run != nil:
		return errors.New("run e fetch nello stesso passo")
	case s.Fetch != nil:
		return s.Fetch.validate()
	case len(s.Run) == 0:
		return errors.New("run vuoto")
	}
	for _, a := range s.Run {
		if a == "" || len([]rune(a)) > 4096 {
			return errors.New("argomento di run vuoto o oltre 4096 caratteri")
		}
	}
	return nil
}

func (f Fetch) validate() error {
	switch {
	case !strings.HasPrefix(f.URL, "https://") || len([]rune(f.URL)) > 2048:
		return fmt.Errorf("fetch.url %q non https", f.URL)
	case !sha256RE.MatchString(f.SHA256):
		return errors.New("fetch.sha256 non è 64 cifre esadecimali minuscole")
	case !DestRE.MatchString(f.Dest):
		return fmt.Errorf("fetch.dest %q non è un nome di file", f.Dest)
	}
	return nil
}

// Canonical produce la forma canonica di un valore JSON (chiavi ordinate, senza spazi, senza escape
// HTML): è la forma che firma chi firma le ricette. Il connettore non la usa per verificare (verifica
// i byte ricevuti); serve ai test e al backend finto per firmare.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
