// Package update è l'aggiornamento automatico del connettore (spec §1.10, ADR 0438 D3, D4, D6).
//
// Due ruoli, due utenti, nessuna fiducia fra i due:
//
//   - Il CONNETTORE (utente non privilegiato, Manager) chiede con update.check, riceve
//     update.available, verifica la firma Ed25519 del manifest canonico con la chiave mt-updates-2026
//     incorporata, piattaforma, minProtocol, versione strettamente maggiore e URL sotto le release
//     del repository; scarica con tetto e timeout, verifica lo SHA-256 e lascia in <state>/update/
//     il binario, il manifest, la firma e apply.request. Non esegue MAI il binario scaricato.
//   - L'UPDATER (root, Apply, eseguito dall'unità systemd col binario installato, cioè quello vecchio)
//     copia binario, manifest e firma in <state>/updater/ (root, 0700) senza seguire symlink e per
//     contenuto, e SU QUELLA COPIA riverifica da sé firma, SHA-256, piattaforma, minProtocol e
//     versione: nulla di ciò che il connettore ha preparato è creduto (decisione BOSS 2026-11-17).
//     Solo allora conserva <bin>.prev, installa con rename atomico, scrive pending.json, arma il timer
//     di ritorno e riavvia il servizio. RollbackCheck (root, dal timer, col binario .prev) ripristina
//     la versione precedente se il connettore nuovo non ha scritto il marcatore connected-<versione>.
//
// La cartella <state>/update/ è la cassetta delle lettere fra i due: il connettore vi scrive la
// richiesta e il marcatore di collegamento, l'updater vi scrive gli esiti (apply.rejected,
// applied.json, rolled-back.json) che il connettore legge e riporta al backend con update.result.
package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/signing"
)

// ReleasesPrefix è il solo prefisso ammesso per l'URL del manifest (ADR 0438 D3): le release del
// repository pubblico. Il confronto è esatto, carattere per carattere.
const ReleasesPrefix = "https://github.com/monsterterminal/connector/releases/download/"

// MaxBinaryBytes è il tetto del download e della copia del binario: 64 MiB (ADR 0438 D3).
const MaxBinaryBytes = 64 << 20

// MaxManifestBytes è il tetto di un manifest o di una firma letti dalla cassetta delle lettere.
const MaxManifestBytes = 64 << 10

// Motivi dei rifiuti e dei salti (ADR 0438 D3 e D6): l'insieme chiuso di update.result.reason.
const (
	ReasonBadSignature   = "bad-signature"
	ReasonNotNewer       = "not-newer"
	ReasonWrongPlatform  = "wrong-platform"
	ReasonProtocolTooOld = "protocol-too-old"
	ReasonBadURL         = "bad-url"
	ReasonDownloadFailed = "download-failed"
	ReasonSHA256Mismatch = "sha256-mismatch"
	ReasonTooLarge       = "too-large"
	ReasonHostConfig     = "host-config"
	ReasonTenantSwitch   = "tenant-switch"
	// ReasonNoWelcome è il motivo del ritorno indietro (rolled-back.json, D3 punto 4).
	ReasonNoWelcome = "no-welcome"
)

// Reasons è l'elenco dei motivi di update.result (failed e skipped), nell'ordine dell'ADR 0438 D3 e D6.
var Reasons = []string{
	ReasonBadSignature, ReasonNotNewer, ReasonWrongPlatform, ReasonProtocolTooOld, ReasonBadURL,
	ReasonDownloadFailed, ReasonSHA256Mismatch, ReasonTooLarge, ReasonHostConfig, ReasonTenantSwitch,
}

// Rejection è un rifiuto con il suo motivo, uno di Reasons.
type Rejection struct {
	Reason string
	Err    error
}

func (r *Rejection) Error() string {
	if r.Err == nil {
		return r.Reason
	}
	return r.Reason + ": " + r.Err.Error()
}

func (r *Rejection) Unwrap() error { return r.Err }

func reject(reason string, format string, a ...any) *Rejection {
	return &Rejection{Reason: reason, Err: fmt.Errorf(format, a...)}
}

// ReasonOf estrae il motivo da un errore; un errore che non è un Rejection vale download-failed se
// fallback è vuoto.
func ReasonOf(err error, fallback string) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Reason
	}
	if fallback == "" {
		return ReasonDownloadFailed
	}
	return fallback
}

// Canonical è la forma canonica del manifest, quella firmata da release.yml: JSON con le chiavi in
// ordine alfabetico, senza spazi né a capo, `security` solo se presente, nessun escape HTML (gli URL
// non ne hanno bisogno e printf della CI non lo fa). La struttura UpdateManifest ha i campi già
// nell'ordine alfabetico delle chiavi.
func Canonical(m protocol.UpdateManifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// VerifySignature verifica la firma Ed25519 grezza (64 byte) del manifest canonico con la chiave
// pubblica data. Un manifest non ricostruibile o una firma di lunghezza diversa sono bad-signature.
func VerifySignature(m protocol.UpdateManifest, signature []byte, key ed25519.PublicKey) error {
	canon, err := Canonical(m)
	if err != nil {
		return reject(ReasonBadSignature, "manifest non serializzabile: %v", err)
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, canon, signature) {
		return reject(ReasonBadSignature, "firma Ed25519 non valida sul manifest canonico")
	}
	return nil
}

// Platform è la piattaforma e il protocollo di chi verifica.
type Platform struct {
	OS, Arch string
	Protocol int
	// Version è la versione in esecuzione: il manifest deve essere strettamente maggiore.
	Version string
}

// ThisPlatform è la piattaforma del binario in esecuzione.
func ThisPlatform(version string) Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH, Protocol: protocol.Version, Version: version}
}

// Verify applica, nell'ordine, i controlli dell'ADR 0438 D3 punto 1 a un manifest ricevuto: firma
// con la chiave data (bad-signature), os/arch (wrong-platform), minProtocol (protocol-too-old),
// versione strettamente maggiore in semver 2.0 con pre-release (not-newer), URL col prefisso esatto
// delle release (bad-url). Il primo controllo che fallisce decide il motivo; la firma viene prima di
// tutto, così un manifest non autentico non dice niente di sé.
func Verify(m protocol.UpdateManifest, signature []byte, key ed25519.PublicKey, p Platform) error {
	if err := VerifySignature(m, signature, key); err != nil {
		return err
	}
	switch {
	case m.OS != p.OS || m.Arch != p.Arch:
		return reject(ReasonWrongPlatform, "manifest per %s/%s, qui %s/%s", m.OS, m.Arch, p.OS, p.Arch)
	case m.MinProtocol > p.Protocol:
		return reject(ReasonProtocolTooOld, "il manifest richiede il protocollo %d, qui %d", m.MinProtocol, p.Protocol)
	case !protocol.IsSemver(m.Version) || !protocol.IsSemver(p.Version) || protocol.CompareSemver(m.Version, p.Version) <= 0:
		return reject(ReasonNotNewer, "versione %s non maggiore di %s", m.Version, p.Version)
	case !strings.HasPrefix(m.URL, ReleasesPrefix) || len(m.URL) == len(ReleasesPrefix):
		return reject(ReasonBadURL, "url fuori dalle release del repository")
	}
	return nil
}

// VerifyWithEmbeddedKey è Verify con la chiave mt-updates-2026 incorporata nel binario.
func VerifyWithEmbeddedKey(m protocol.UpdateManifest, signature []byte, p Platform) error {
	key, err := signing.PublicKey(signing.Updates)
	if err != nil {
		return reject(ReasonBadSignature, "chiave degli aggiornamenti: %v", err)
	}
	return Verify(m, signature, key, p)
}

// Decide applica gli interruttori (ADR 0438 D6): si aggiorna solo se il file locale dell'host E
// l'interruttore del backend sono accesi. `security` nel manifest ignora SOLO quello del backend; il
// file locale resta assoluto, anche per la sicurezza. Il motivo restituito è quello di update.result
// skipped; vuoto = si procede.
func Decide(hostAutoUpdate, backendAutoUpdate, security bool) (skipReason string) {
	switch {
	case !hostAutoUpdate:
		return ReasonHostConfig
	case !backendAutoUpdate && !security:
		return ReasonTenantSwitch
	}
	return ""
}
