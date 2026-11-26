package protocol

// Catalogo di ricette firmate, schemi 0.11.0 (spec §1.9, ADR 0410 A4). Il backend manda
// catalog.install con la ricetta così come è stata firmata; il connettore verifica, esegue i passi con
// il proprio utente e risponde con catalog.install.progress (dopo ogni passo) e catalog.install.result.
// A esito ok scrive nel giornale l'evento catalog.installed.

import (
	"encoding/json"
	"strings"
)

const (
	TypeCatalogInstall         = "catalog.install"
	TypeCatalogInstallProgress = "catalog.install.progress"
	TypeCatalogInstallResult   = "catalog.install.result"
	KindCatalogInstalled       = "catalog.installed"
)

// CapabilityCatalogInstall è la capacità dichiarata in hello dal connettore che sa eseguire le
// ricette, solo con catalog_install=yes (§1.11, ADR 0410 A4).
const CapabilityCatalogInstall = "catalog-install"

// Esiti di catalog.install.result (enum dello schema, nello stesso ordine).
const (
	CatalogOK      = "ok"
	CatalogFailed  = "failed"
	CatalogRefused = "refused"
)

// CatalogOutcomes è l'enum di catalog.install.result.outcome.
var CatalogOutcomes = []string{CatalogOK, CatalogFailed, CatalogRefused}

// Motivi di catalog.install.result (enum chiuso dalla 0.11.0, §1.9).
const (
	ReasonDisabledOnHost   = "disabled-on-host"
	ReasonSignatureInvalid = "signature-invalid"
	ReasonExpired          = "expired"
	ReasonPlatformMismatch = "platform-mismatch"
	ReasonRequiresFailed   = "requires-failed"
	ReasonDowngrade        = "downgrade"
	ReasonRootNotAllowed   = "root-not-allowed"
	ReasonChecksumMismatch = "checksum-mismatch"
	ReasonStepFailed       = "step-failed"
	ReasonTimeout          = "timeout"
	ReasonBusy             = "busy"
)

// CatalogReasons è l'enum di catalog.install.result.reason, nello stesso ordine dello schema.
var CatalogReasons = []string{
	ReasonDisabledOnHost, ReasonSignatureInvalid, ReasonExpired, ReasonPlatformMismatch,
	ReasonRequiresFailed, ReasonDowngrade, ReasonRootNotAllowed, ReasonChecksumMismatch,
	ReasonStepFailed, ReasonTimeout, ReasonBusy,
}

// FailedReasons sono i motivi che accompagnano failed (gli altri accompagnano refused).
var FailedReasons = []string{ReasonChecksumMismatch, ReasonStepFailed, ReasonTimeout}

// Tetti dello schema 0.11.0.
const (
	MaxCatalogDetail          = 512
	MaxCatalogLogTail         = 8192
	MaxCatalogProgressMessage = 500
	MaxCatalogSteps           = 32
)

// CatalogInstall è il corpo di catalog.install. Recipe resta nei byte ricevuti: la firma si verifica
// su quelli, mai su una riserializzazione (§1.9 punto 2).
type CatalogInstall struct {
	InstallID  string          `json:"installId"`
	Recipe     json.RawMessage `json:"recipe"`
	Signature  string          `json:"signature"`
	ApprovedBy string          `json:"approvedBy"`
	ApprovedAt string          `json:"approvedAt"`
}

// DecodeCatalogInstall decodifica la busta di catalog.install: oggetto chiuso, installId e approvedBy
// UUID, approvedAt istante UTC, recipe un oggetto. La ricetta e la firma NON si giudicano qui: una
// ricetta non valida o una firma malformata sono un rifiuto signature-invalid del catalogo, non un
// messaggio non valido, così il backend riceve l'esito dell'installazione.
func DecodeCatalogInstall(body []byte) (CatalogInstall, error) {
	var c CatalogInstall
	if err := DecodeStrict(body, &c); err != nil {
		return c, err
	}
	switch {
	case !uuidRE.MatchString(c.InstallID):
		return c, invalid("installId non è un UUID v4")
	case !uuidRE.MatchString(c.ApprovedBy):
		return c, invalid("approvedBy non è un UUID v4")
	case len(c.Recipe) == 0 || c.Recipe[0] != '{':
		return c, invalid("recipe mancante o non oggetto")
	}
	if err := validTimestamp(c.ApprovedAt); err != nil {
		return c, err
	}
	return c, nil
}

// CatalogInstallProgress è il corpo di catalog.install.progress: Step conta da 1.
type CatalogInstallProgress struct {
	InstallID string `json:"installId"`
	Step      int    `json:"step"`
	Of        int    `json:"of"`
	Message   string `json:"message"`
}

// CatalogInstallResult è il corpo di catalog.install.result. StepIndex conta da 0 e c'è solo con i
// motivi di failed; Reason manca con ok.
type CatalogInstallResult struct {
	InstallID string `json:"installId"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
	StepIndex *int   `json:"stepIndex,omitempty"`
	LogTail   string `json:"logTail,omitempty"`
}

// CatalogInstalled è il data dell'evento catalog.installed (solo a esito ok).
type CatalogInstalled struct {
	RecipeID      string `json:"recipeId"`
	RecipeVersion int    `json:"recipeVersion"`
	Agent         string `json:"agent"`
	Version       string `json:"version"`
}

// TruncateRunes taglia s a non più di max caratteri (gli schemi contano i caratteri) e lo rende UTF-8
// valido. Tiene l'INIZIO.
func TruncateRunes(s string, max int) string {
	s = strings.ToValidUTF8(s, "?")
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// TailBytes tiene la CODA di s in al più max byte, in UTF-8 valido (logTail, §1.9: ≤ 8 KiB). Una
// sequenza di byte non validi diventa un solo «?»: la lunghezza non cresce mai.
func TailBytes(s string, max int) string {
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return strings.ToValidUTF8(s, "?")
}
