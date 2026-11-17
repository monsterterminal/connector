package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/signing"
)

// Nomi delle unità systemd scritte da install.sh.
const (
	ServiceName   = "monsterterminal-connector"
	RollbackTimer = "monsterterminal-connector-rollback.timer"
	// PrevSuffix è il suffisso della copia del binario precedente accanto a quello installato.
	PrevSuffix = ".prev"
	// NewSuffix è il suffisso del file scritto prima della rinomina atomica.
	NewSuffix = ".new"
	// UpdaterSubdir e UpdateSubdir sono le due cartelle sotto <state>.
	UpdaterSubdir = "updater"
	UpdateSubdir  = "update"
)

// Systemctl esegue `systemctl args...`; sostituibile nei test.
type Systemctl func(args ...string) error

// RunSystemctl è il Systemctl vero.
func RunSystemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %v: %s", args, err, string(out))
	}
	return nil
}

// ApplyConfig configura l'updater (root).
type ApplyConfig struct {
	// StateDir è /var/lib/monsterterminal: sotto ci sono update/ (del connettore) e updater/ (root).
	StateDir string
	// Binary è il percorso del binario installato (/usr/local/bin/monsterterminal-connector).
	Binary string
	// Running è la versione del binario che esegue apply-update: quella installata, nota buona.
	Running string
	// Platform è la piattaforma di chi applica; vuota = quella del binario in esecuzione.
	Platform Platform
	// Key è la chiave pubblica per la riverifica; nil = mt-updates-2026 incorporata.
	Key ed25519.PublicKey
	// Systemctl; nil = il vero systemctl.
	Systemctl Systemctl
	// UID è il proprietario atteso di updater/; -1 = l'utente che esegue (root nell'unità).
	UID    int
	Logger *slog.Logger
	Now    func() time.Time
}

func (c *ApplyConfig) defaults() error {
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Systemctl == nil {
		c.Systemctl = RunSystemctl
	}
	if c.Platform == (Platform{}) {
		c.Platform = ThisPlatform(c.Running)
	}
	if c.UID < 0 {
		c.UID = os.Getuid()
	}
	if c.Key == nil {
		k, err := signing.PublicKey(signing.Updates)
		if err != nil {
			return err
		}
		c.Key = k
	}
	if c.StateDir == "" || c.Binary == "" {
		return errors.New("StateDir e Binary sono obbligatori")
	}
	return nil
}

// Apply è `apply-update`: l'unità root che installa ciò che il connettore ha preparato in
// <state>/update/, SENZA fidarsene (ADR 0438 D3 punto 2, decisione BOSS 2026-11-17).
//
//  1. apply.request si toglie in ogni caso (anche su rifiuto), così l'unità path non riparte.
//  2. <state>/updater/ è root 0700, creata qui; se esiste con un altro proprietario o altri permessi
//     ci si ferma.
//  3. Manifest, firma e binario si copiano da update/ a updater/ aprendo i sorgenti con O_NOFOLLOW,
//     solo se sono file regolari (Lstat + fstat), per contenuto e col tetto: mai rename di un file
//     dell'utente, mai seguire un symlink.
//  4. SULLA COPIA si riverificano firma Ed25519 del manifest canonico con la chiave incorporata,
//     SHA-256 del binario contro il manifest, piattaforma, minProtocol e versione > Running.
//  5. Solo allora: <bin>.prev (copia del binario attuale), <bin>.new root 0755 + rename atomico,
//     pending.json in updater/ e applied.json nella cassetta del connettore, timer di ritorno armato
//     e servizio riavviato.
//
// Un rifiuto scrive apply.rejected (versione, motivo) nella cassetta del connettore, che lo riporta
// come update.result failed; un binario non firmato, firmato con un'altra chiave o con sha diverso
// non si installa MAI. Gli errori di installazione (copie, rename, systemctl) si riportano col motivo
// download-failed: il pacchetto preparato non è installabile.
func Apply(cfg ApplyConfig) (err error) {
	if err := cfg.defaults(); err != nil {
		return err
	}
	log := cfg.Logger
	updateDir := filepath.Join(cfg.StateDir, UpdateSubdir)
	updaterDir := filepath.Join(cfg.StateDir, UpdaterSubdir)
	// 1. La richiesta si consuma sempre.
	requested, reqErr := ReadRequestVersion(updateDir)
	defer os.Remove(filepath.Join(updateDir, FileApplyRequest))
	if reqErr != nil {
		log.Warn("apply.request illeggibile", "errore", reqErr.Error())
	}
	version := requested
	rejected := func(reason string, cause error) error {
		log.Error("aggiornamento rifiutato dall'updater", "versione", version, "motivo", reason, "errore", cause.Error())
		if werr := WriteToMailbox(updateDir, FileApplyRejected, Outcome{Version: version, Reason: reason}); werr != nil {
			log.Error("apply.rejected non scritto", "errore", werr.Error())
		}
		return &Rejection{Reason: reason, Err: cause}
	}
	// 2. La cartella root-only.
	if err := ensureOwnedDir(updaterDir, cfg.UID); err != nil {
		return fmt.Errorf("cartella dell'updater: %w", err)
	}
	defer func() {
		for _, n := range []string{FileBinary, FileManifest, FileSignature} {
			_ = os.Remove(filepath.Join(updaterDir, n))
		}
	}()
	// 3. Le copie, senza fiducia.
	manCopy := filepath.Join(updaterDir, FileManifest)
	if _, err := copyNoFollow(filepath.Join(updateDir, FileManifest), manCopy, MaxManifestBytes, 0o600); err != nil {
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("manifest: %w", err))
	}
	sigCopy := filepath.Join(updaterDir, FileSignature)
	if _, err := copyNoFollow(filepath.Join(updateDir, FileSignature), sigCopy, MaxManifestBytes, 0o600); err != nil {
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("firma: %w", err))
	}
	binCopy := filepath.Join(updaterDir, FileBinary)
	binSHA, err := copyNoFollow(filepath.Join(updateDir, FileBinary), binCopy, MaxBinaryBytes, 0o755)
	if err != nil {
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("binario: %w", err))
	}
	// 4. La riverifica, sulla copia (letta da updater/, che è nostra).
	manRaw, err := readSmallNoFollow(manCopy, MaxManifestBytes)
	if err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("manifest copiato: %w", err))
	}
	sigRaw, err := readSmallNoFollow(sigCopy, MaxManifestBytes)
	if err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("firma copiata: %w", err))
	}
	var m protocol.UpdateManifest
	if err := protocol.DecodeStrict(manRaw, &m); err != nil {
		return rejected(ReasonBadSignature, fmt.Errorf("manifest: %w", err))
	}
	if err := protocol.ValidateUpdateManifest(m); err != nil {
		return rejected(ReasonBadSignature, fmt.Errorf("manifest: %w", err))
	}
	if m.Version != "" {
		version = m.Version
	}
	if len(sigRaw) != ed25519.SignatureSize {
		return rejected(ReasonBadSignature, fmt.Errorf("firma di %d byte, attesi %d", len(sigRaw), ed25519.SignatureSize))
	}
	if err := Verify(m, sigRaw, cfg.Key, cfg.Platform); err != nil {
		return rejected(ReasonOf(err, ReasonBadSignature), err)
	}
	if binSHA != m.SHA256 {
		return rejected(ReasonSHA256Mismatch, fmt.Errorf("sha256 del binario %s, manifest %s", binSHA, m.SHA256))
	}
	if requested != "" && requested != m.Version {
		log.Warn("apply.request e manifest non concordano sulla versione", "richiesta", requested, "manifest", m.Version)
	}
	// 5. L'installazione.
	log.Info("aggiornamento verificato dall'updater: installo", "versione", m.Version, "precedente", cfg.Running)
	prev := cfg.Binary + PrevSuffix
	if _, err := copyNoFollow(cfg.Binary, prev, MaxBinaryBytes, 0o755); err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("copia del binario attuale in %s: %w", prev, err))
	}
	newPath := cfg.Binary + NewSuffix
	if _, err := copyNoFollow(binCopy, newPath, MaxBinaryBytes, 0o755); err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("scrittura di %s: %w", newPath, err))
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		_ = os.Remove(newPath)
		return rejected(ReasonDownloadFailed, fmt.Errorf("permessi di %s: %w", newPath, err))
	}
	p := Pending{Version: m.Version, PreviousVersion: cfg.Running, At: protocol.Timestamp(cfg.Now())}
	if err := WritePending(updaterDir, p); err != nil {
		_ = os.Remove(newPath)
		return rejected(ReasonDownloadFailed, fmt.Errorf("pending.json: %w", err))
	}
	// Il timer di ritorno si arma PRIMA dello scambio: senza rete di sicurezza non si installa.
	if err := cfg.Systemctl("restart", RollbackTimer); err != nil {
		_ = os.Remove(newPath)
		_ = os.Remove(filepath.Join(updaterDir, FilePending))
		return rejected(ReasonDownloadFailed, fmt.Errorf("timer di ritorno: %w", err))
	}
	if err := os.Rename(newPath, cfg.Binary); err != nil {
		_ = os.Remove(newPath)
		_ = os.Remove(filepath.Join(updaterDir, FilePending))
		_ = cfg.Systemctl("stop", RollbackTimer)
		return rejected(ReasonDownloadFailed, fmt.Errorf("rinomina su %s: %w", cfg.Binary, err))
	}
	if err := WriteToMailbox(updateDir, FileApplied, p); err != nil {
		// Il binario è già installato: non si torna indietro per un file di esito. Il connettore
		// nuovo non manderà `ok`, il timer farà il suo lavoro se davvero non si collega.
		log.Error("applied.json non scritto", "errore", err.Error())
	}
	log.Info("binario installato, riavvio il servizio", "versione", m.Version, "binario", cfg.Binary)
	if err := cfg.Systemctl("restart", ServiceName); err != nil {
		log.Error("riavvio del servizio non riuscito: il timer di ritorno deciderà", "errore", err.Error())
		return err
	}
	return nil
}

// RollbackAction è la decisione di rollback-check.
type RollbackAction int

const (
	// RollbackNothing: nessun aggiornamento in attesa.
	RollbackNothing RollbackAction = iota
	// RollbackDisarm: il connettore nuovo si è collegato; si toglie la rete di sicurezza.
	RollbackDisarm
	// RollbackRestore: nessun welcome; si ripristina il binario precedente.
	RollbackRestore
)

func (a RollbackAction) String() string {
	switch a {
	case RollbackDisarm:
		return "disarmo"
	case RollbackRestore:
		return "ritorno"
	}
	return "niente"
}

// DecideRollback è la decisione pura di rollback-check (ADR 0438 D3 punti 3 e 4): con un
// aggiornamento in attesa, il marcatore connected-<versione> disarma, la sua assenza ripristina.
func DecideRollback(pending, connected bool) RollbackAction {
	switch {
	case !pending:
		return RollbackNothing
	case connected:
		return RollbackDisarm
	}
	return RollbackRestore
}

// RollbackCheck è `rollback-check`: eseguito dal timer, come root, col binario .prev. Legge
// pending.json; se il connettore nuovo ha scritto connected-<versione> toglie pending.json, .prev e
// il marcatore e ferma il timer; altrimenti rimette .prev al posto del binario (rename atomico),
// scrive rolled-back.json (versione tentata, motivo no-welcome) e riavvia il servizio.
func RollbackCheck(cfg ApplyConfig) (RollbackAction, error) {
	if err := cfg.defaults(); err != nil {
		return RollbackNothing, err
	}
	log := cfg.Logger
	updateDir := filepath.Join(cfg.StateDir, UpdateSubdir)
	updaterDir := filepath.Join(cfg.StateDir, UpdaterSubdir)
	if err := ensureOwnedDir(updaterDir, cfg.UID); err != nil {
		return RollbackNothing, fmt.Errorf("cartella dell'updater: %w", err)
	}
	p, ok, err := ReadPending(updaterDir)
	if err != nil {
		return RollbackNothing, err
	}
	prev := cfg.Binary + PrevSuffix
	action := DecideRollback(ok, ok && ConnectedExists(updateDir, p.Version))
	switch action {
	case RollbackNothing:
		log.Info("nessun aggiornamento in attesa: niente da fare")
	case RollbackDisarm:
		log.Info("il connettore nuovo si è collegato: disarmo il ritorno", "versione", p.Version)
		for _, f := range []string{filepath.Join(updaterDir, FilePending), prev, ConnectedMarker(updateDir, p.Version)} {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Warn("file non rimosso", "file", f, "errore", err.Error())
			}
		}
		_ = cfg.Systemctl("stop", RollbackTimer)
	case RollbackRestore:
		log.Error("il connettore nuovo non si è collegato in tempo: ripristino la versione precedente", "tentata", p.Version, "precedente", p.PreviousVersion)
		if err := os.Rename(prev, cfg.Binary); err != nil {
			return action, fmt.Errorf("ripristino di %s: %w", cfg.Binary, err)
		}
		if err := WriteToMailbox(updateDir, FileRolledBack, Outcome{Version: p.Version, Reason: ReasonNoWelcome}); err != nil {
			log.Error("rolled-back.json non scritto", "errore", err.Error())
		}
		_ = os.Remove(filepath.Join(updateDir, FileApplied))
		_ = os.Remove(filepath.Join(updaterDir, FilePending))
		if err := cfg.Systemctl("restart", ServiceName); err != nil {
			return action, err
		}
	}
	return action, nil
}

// ensureOwnedDir crea dir (0700) se manca; se esiste deve essere una cartella (non un symlink) di uid
// con permessi 0700, altrimenti errore: l'updater non lavora in una cartella che non controlla.
func ensureOwnedDir(dir string, uid int) error {
	st, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
		st, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s non è una cartella", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != uid {
		return fmt.Errorf("%s appartiene a uid %d, atteso %d", dir, sys.Uid, uid)
	}
	if st.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s ha permessi %o, attesi 0700", dir, st.Mode().Perm())
	}
	return nil
}

// copyNoFollow copia src in dst per contenuto, senza mai seguire un symlink: Lstat e apertura con
// O_NOFOLLOW sul sorgente, che deve essere un file regolare entro max byte (oltre: too-large); dst
// si toglie e si ricrea con O_EXCL e il modo dato. Restituisce lo SHA-256 esadecimale di ciò che ha
// copiato. Un sorgente assente, non regolare o illeggibile è download-failed: ciò che il connettore
// ha preparato non è utilizzabile.
func copyNoFollow(src, dst string, max int64, mode fs.FileMode) (string, error) {
	st, err := os.Lstat(src)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%v", err)
	}
	if !st.Mode().IsRegular() {
		return "", reject(ReasonDownloadFailed, "%s non è un file regolare (%s)", filepath.Base(src), st.Mode().Type())
	}
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NOCTTY, 0)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%v", err)
	}
	defer f.Close()
	// fstat sul descrittore aperto: chiude la finestra fra Lstat e open.
	fst, err := f.Stat()
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%v", err)
	}
	if !fst.Mode().IsRegular() {
		return "", reject(ReasonDownloadFailed, "%s non è un file regolare", filepath.Base(src))
	}
	if fst.Size() > max {
		return "", reject(ReasonTooLarge, "%s: %d byte, oltre %d", filepath.Base(src), fst.Size(), max)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", reject(ReasonDownloadFailed, "%v", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%v", err)
	}
	fail := func(reason, format string, a ...any) (string, error) {
		out.Close()
		os.Remove(dst)
		return "", reject(reason, format, a...)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, max+1))
	if err != nil {
		return fail(ReasonDownloadFailed, "copia di %s: %v", filepath.Base(src), err)
	}
	if n > max {
		return fail(ReasonTooLarge, "%s oltre %d byte", filepath.Base(src), max)
	}
	if err := out.Sync(); err != nil {
		return fail(ReasonDownloadFailed, "sync di %s: %v", filepath.Base(dst), err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return "", reject(ReasonDownloadFailed, "chiusura di %s: %v", filepath.Base(dst), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
