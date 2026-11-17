package protocol

// Aggiornamento automatico del connettore, schemi 0.10.0 (spec §1.10, ADR 0438). I messaggi
// update.available e update.result esistono dalla 0.1.0; update.check e update.none nascono con la
// 0.10.0 (D4). Il manifest è firmato sulla forma canonica: chiavi in ordine alfabetico, senza spazi,
// `security` presente solo quando chi firma lo ha scritto (D2). Nessun messaggio esegue nulla: il
// connettore scarica e prepara, un'unità root applica dopo aver riverificato da sé (D3).

import (
	"encoding/base64"
	"regexp"
	"strings"
)

const (
	TypeUpdateCheck     = "update.check"
	TypeUpdateNone      = "update.none"
	TypeUpdateAvailable = "update.available"
	TypeUpdateResult    = "update.result"
)

// Esiti di update.result (enum dello schema, nello stesso ordine).
const (
	UpdateOK         = "ok"
	UpdateFailed     = "failed"
	UpdateSkipped    = "skipped"
	UpdateRolledBack = "rolled-back"
)

// UpdateOutcomes è l'enum di update.result.outcome, nello stesso ordine dello schema.
var UpdateOutcomes = []string{UpdateOK, UpdateFailed, UpdateSkipped, UpdateRolledBack}

// MaxUpdateReasonLength è il tetto di update.result.reason (schema).
const MaxUpdateReasonLength = 200

// UpdateManifest è il manifest di aggiornamento (objects/update-manifest.json). L'ORDINE dei campi è
// quello alfabetico delle chiavi JSON: così la serializzazione con encoding/json produce la forma
// canonica firmata da release.yml (`arch, minProtocol, os, [security], sha256, url, version`).
// Security è un puntatore perché entra nella forma canonica solo se presente (ADR 0438 D2).
type UpdateManifest struct {
	Arch        string `json:"arch"`
	MinProtocol int    `json:"minProtocol"`
	OS          string `json:"os"`
	Security    *bool  `json:"security,omitempty"`
	SHA256      string `json:"sha256"`
	URL         string `json:"url"`
	Version     string `json:"version"`
}

// IsSecurity dice se il manifest è contrassegnato come aggiornamento di sicurezza.
func (m UpdateManifest) IsSecurity() bool { return m.Security != nil && *m.Security }

// UpdateAvailable è il corpo di update.available (backend → connettore).
type UpdateAvailable struct {
	Version   string         `json:"version"`
	Manifest  UpdateManifest `json:"manifest"`
	Signature string         `json:"signature"`
}

// UpdateResult è il corpo di update.result (connettore → backend).
type UpdateResult struct {
	Version string `json:"version"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

var (
	sha256HexUpdateRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// signatureRE è common.json#signature: 64 byte in base64 standard, 86 caratteri più «==».
	signatureRE = regexp.MustCompile(`^[A-Za-z0-9+/]{86}==$`)
)

// DecodeUpdateAvailable decodifica e valida il corpo di update.available contro lo schema: oggetto
// chiuso, manifest chiuso, versione semver, os/arch nell'enum, url https, sha256 esadecimale,
// minProtocol ≥ 1, firma nella forma dello schema, e version del corpo uguale a quella del manifest.
// La firma NON si verifica qui: lo fa internal/update con la chiave incorporata.
func DecodeUpdateAvailable(body []byte) (UpdateAvailable, error) {
	var u UpdateAvailable
	if err := DecodeStrict(body, &u); err != nil {
		return u, err
	}
	if err := ValidateUpdateManifest(u.Manifest); err != nil {
		return u, err
	}
	switch {
	case !IsSemver(u.Version):
		return u, invalid("version non è semver")
	case u.Version != u.Manifest.Version:
		return u, invalid("version del messaggio diversa da manifest.version")
	case !signatureRE.MatchString(u.Signature):
		return u, invalid("signature non è una firma Ed25519 in base64")
	}
	return u, nil
}

// ValidateUpdateManifest applica i vincoli di objects/update-manifest.json a un manifest già
// decodificato: versione semver, os/arch nell'enum, url https, sha256 esadecimale, minProtocol ≥ 1.
func ValidateUpdateManifest(m UpdateManifest) error {
	switch {
	case !IsSemver(m.Version):
		return invalid("manifest.version non è semver")
	case m.OS != "linux":
		return invalid("manifest.os fuori dall'enum")
	case m.Arch != "amd64" && m.Arch != "arm64":
		return invalid("manifest.arch fuori dall'enum")
	case !strings.HasPrefix(m.URL, "https://"):
		return invalid("manifest.url non è https")
	case !sha256HexUpdateRE.MatchString(m.SHA256):
		return invalid("manifest.sha256 non è 64 cifre esadecimali")
	case m.MinProtocol < 1:
		return invalid("manifest.minProtocol sotto 1")
	}
	return nil
}

// DecodeSignature legge la firma in base64 (common.json#signature) nei suoi 64 byte.
func DecodeSignature(s string) ([]byte, error) {
	if !signatureRE.MatchString(s) {
		return nil, invalid("signature non è una firma Ed25519 in base64")
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != 64 {
		return nil, invalid("signature non decodificabile")
	}
	return raw, nil
}

// DecodeUpdateNone valida il corpo (vuoto) di update.none.
func DecodeUpdateNone(body []byte) error {
	var empty struct{}
	return DecodeStrict(body, &empty)
}
