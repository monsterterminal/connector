package catalog

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Refusal è un rifiuto prima di eseguire qualunque passo: motivo dell'enum e dettaglio facoltativo.
type Refusal struct {
	Reason string
	Detail string
}

func (r *Refusal) Error() string {
	if r.Detail == "" {
		return r.Reason
	}
	return r.Reason + ": " + r.Detail
}

// HostPlatform è la piattaforma dell'host: os, arch e famiglia di distribuzione come in hello.
type HostPlatform struct {
	OS, Arch, Distro string
}

// Env è tutto ciò che le verifiche leggono dall'host. Ogni campo è sostituibile nei test.
type Env struct {
	// CatalogInstall è catalog_install del file dell'host (§1.11): l'interruttore del cliente.
	CatalogInstall bool
	// Key è la chiave pubblica delle ricette; nil = mt-recipes-2026 incorporata.
	Key ed25519.PublicKey
	// Now è l'orologio per expiresAt.
	Now func() time.Time
	// Platform è la piattaforma dell'host.
	Platform HostPlatform
	// Root dice se il connettore gira come root.
	Root bool
	// LookupVersion cerca un comando per nome e ne legge la versione (`<comando> --version`, tetto
	// 5 s): found=false se il comando non c'è; version vuota se c'è ma non ha detto una versione.
	LookupVersion func(name string) (version string, found bool)
	// Installed legge dal file di stato l'ultima recipeVersion installata di una ricetta; ok=false se
	// la ricetta non è mai stata installata qui.
	Installed func(recipeID string) (version int, ok bool, err error)
	// Acquire prende il turno dell'unica installazione ammessa per host; false = occupato. Chi riceve
	// true deve chiamare il rilascio restituito da Check a fine corsa.
	Acquire func() bool
}

// Check verifica una ricetta nell'ordine di §1.9 e si ferma al primo rifiuto:
//
//  1. interruttore dell'host (disabled-on-host)
//  2. firma Ed25519 sui byte ricevuti, con la chiave delle ricette (signature-invalid)
//  3. scadenza (expired)
//  4. piattaforma: os, arch, distribuzione (platform-mismatch)
//  5. root (root-not-allowed)
//  6. requisiti, con la versione minima (requires-failed, con requisito e versione trovata in detail)
//  7. downgrade dal file di stato (downgrade)
//  8. occupato (busy)
//
// Se tutto passa restituisce la ricetta e il turno preso (Acquire), che il chiamante rilascia.
func Check(env Env, raw []byte, signature string) (Recipe, *Refusal) {
	if !env.CatalogInstall {
		return Recipe{}, &Refusal{Reason: protocol.ReasonDisabledOnHost, Detail: "catalog_install=no nel file di configurazione dell'host"}
	}
	r, err := ParseRecipe(raw, signature, env.Key)
	if err != nil {
		return Recipe{}, &Refusal{Reason: protocol.ReasonSignatureInvalid, Detail: detail(err.Error())}
	}
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	exp, _ := time.Parse(time.RFC3339Nano, r.ExpiresAt) // già validato da ParseRecipe
	if !now().Before(exp) {
		return r, &Refusal{Reason: protocol.ReasonExpired, Detail: "scaduta il " + r.ExpiresAt}
	}
	if !platformOK(r.Platforms, env.Platform) {
		return r, &Refusal{Reason: protocol.ReasonPlatformMismatch, Detail: detail(fmt.Sprintf("host %s/%s/%s", env.Platform.OS, env.Platform.Arch, env.Platform.Distro))}
	}
	if env.Root {
		return r, &Refusal{Reason: protocol.ReasonRootNotAllowed, Detail: "il connettore gira come root: reinstallarlo con install.sh --user"}
	}
	for _, q := range r.Requires {
		if ref := checkRequire(env, q); ref != nil {
			return r, ref
		}
	}
	if env.Installed != nil {
		v, ok, err := env.Installed(r.RecipeID)
		if err != nil {
			// Un file di stato illeggibile non autorizza a reinstallare sopra: vale come downgrade.
			return r, &Refusal{Reason: protocol.ReasonDowngrade, Detail: detail("file di stato illeggibile: " + err.Error())}
		}
		if ok && r.RecipeVersion <= v {
			return r, &Refusal{Reason: protocol.ReasonDowngrade, Detail: fmt.Sprintf("recipeVersion %d, installata %d", r.RecipeVersion, v)}
		}
	}
	if env.Acquire != nil && !env.Acquire() {
		return r, &Refusal{Reason: protocol.ReasonBusy, Detail: "un'altra installazione è in corso su questo host"}
	}
	return r, nil
}

func platformOK(ps []Platform, h HostPlatform) bool {
	for _, p := range ps {
		if p.OS == h.OS && in(h.Arch, p.Arch) && in(h.Distro, p.Distro) {
			return true
		}
	}
	return false
}

func checkRequire(env Env, q Require) *Refusal {
	want := q.Name
	if q.MinVersion != "" {
		want += " >= " + q.MinVersion
	}
	if env.LookupVersion == nil {
		return &Refusal{Reason: protocol.ReasonRequiresFailed, Detail: detail(want + ", non verificabile")}
	}
	v, found := env.LookupVersion(q.Name)
	switch {
	case !found:
		return &Refusal{Reason: protocol.ReasonRequiresFailed, Detail: detail(want + ", non trovato")}
	case q.MinVersion == "":
		return nil
	case v == "" || !semverRE.MatchString(v):
		return &Refusal{Reason: protocol.ReasonRequiresFailed, Detail: detail(want + ", versione illeggibile")}
	case protocol.CompareSemver(v, q.MinVersion) < 0:
		return &Refusal{Reason: protocol.ReasonRequiresFailed, Detail: detail(want + ", trovato " + v)}
	}
	return nil
}

// detail porta un testo nel tetto di catalog.install.result.detail.
func detail(s string) string {
	return protocol.TruncateRunes(strings.TrimSpace(s), protocol.MaxCatalogDetail)
}

// IsFailedReason dice se il motivo accompagna failed (e non refused).
func IsFailedReason(reason string) bool { return in(reason, protocol.FailedReasons) }
