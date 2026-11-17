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
	"time"

	"github.com/monsterterminal/connector/internal/hostconf"
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
	// UpdateSubdir è la cassetta delle lettere sotto <state> (dell'utente del connettore).
	UpdateSubdir = "update"
	// DefaultConfigFile è il file delle scelte del cliente sull'host (§1.11), letto anche dall'updater.
	DefaultConfigFile = "/etc/monsterterminal/connector.conf"
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
	// StateDir è /var/lib/monsterterminal: dell'utente del connettore; sotto c'è update/.
	StateDir string
	// UpdaterDir è la cartella dell'updater, FUORI dall'albero dell'utente (ADR 0438 A1):
	// vuota = UpdaterDirFor(StateDir), cioè /var/lib/monsterterminal-updater.
	UpdaterDir string
	// TrustedBase è l'antenato da cui in su non si controlla più la proprietà (vuoto = "/": si
	// controlla tutto; i test lo mettono alla loro cartella temporanea).
	TrustedBase string
	// ConfigFile è il file delle scelte del cliente: auto_update = no ferma anche l'updater
	// (D6 «assoluto»); vuoto = DefaultConfigFile. Un file illeggibile vale «spento».
	ConfigFile string
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
	// UID è il proprietario atteso della cartella dell'updater e dei suoi antenati; -1 = l'utente che
	// esegue (root nell'unità).
	UID int
	// ServiceUID è l'utente del servizio del connettore (User= dell'unità, dall'opzione -service-user;
	// root se il servizio gira come root): <state> e update/ devono essere suoi. -1 = l'owner di
	// <state>, che install.sh crea per l'utente del servizio sotto un genitore di root. Il valore
	// zero vale root: chi non lo imposta pretende una cartella di root, cioè si chiude.
	ServiceUID int
	Logger     *slog.Logger
	Now        func() time.Time
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
	if c.UpdaterDir == "" {
		c.UpdaterDir = UpdaterDirFor(c.StateDir)
	}
	if c.TrustedBase == "" {
		c.TrustedBase = "/"
	}
	if c.ConfigFile == "" {
		c.ConfigFile = DefaultConfigFile
	}
	return nil
}

// Apply è `apply-update`: l'unità root che installa ciò che il connettore ha preparato in
// <state>/update/, SENZA fidarsene (ADR 0438 D3 punto 2 e A1, decisione BOSS 2026-11-17).
//
//  1. La cassetta <state>/update/ e la cartella dell'updater si aprono UNA volta come os.Root e da lì
//     in poi ogni lettura, scrittura e cancellazione passa dai descrittori, mai più dal percorso:
//     chi scrive sul genitore non può scambiare una cartella fra un controllo e l'uso. La cartella
//     dell'updater sta fuori dall'albero dell'utente (/var/lib/monsterterminal-updater, root 0700,
//     ogni antenato di root e non scrivibile da altri).
//  2. apply.request si toglie in ogni caso (anche su rifiuto), così l'unità path non riparte.
//  3. auto_update = no nel file dell'host (D6, «assoluto») ferma anche l'updater: reason host-config.
//  4. Manifest, firma e binario si copiano da update/ nella cartella dell'updater senza seguire link,
//     senza bloccarsi (O_NONBLOCK: una FIFO si apre e si scopre), solo se file regolari, col tetto.
//  5. SULLA COPIA si riverificano firma Ed25519 del manifest canonico con la chiave incorporata,
//     SHA-256, piattaforma, minProtocol e versione > Running; e lo SHA-256 della copia FINALE,
//     quella scritta in <bin>.new, si confronta di nuovo col manifest prima della rinomina.
//  6. Solo allora: <bin>.prev, <bin>.new root 0755 + rename atomico, pending.json e applied.json,
//     timer di ritorno armato e servizio riavviato.
//
// Ogni rifiuto, anche per un guasto dell'updater stesso, scrive apply.rejected (versione, motivo) nella
// cassetta: il connettore lo riporta come update.result failed (o skipped per host-config). Un binario
// non firmato, firmato con un'altra chiave o con sha diverso non si installa MAI. Gli errori di
// installazione (copie, rename, systemctl, cartelle) hanno motivo download-failed.
func Apply(cfg ApplyConfig) (err error) {
	if err := cfg.defaults(); err != nil {
		return err
	}
	log := cfg.Logger
	mb, err := OpenMailboxUnder(cfg.StateDir, cfg.ServiceUID)
	if err != nil {
		// Senza cassetta non si può dire di no con apply.rejected: resta il log. Ma apply.request va
		// tolto, se <state> è davvero dell'utente del servizio, altrimenti l'unità path ripartirebbe
		// in ciclo (oltre, il limite di avvio dell'unità lo ferma comunque).
		if rerr := RemoveRequestBestEffort(cfg.StateDir, cfg.ServiceUID); rerr != nil {
			log.Error("apply.request non tolto", "errore", rerr.Error())
		}
		return fmt.Errorf("cassetta delle lettere: %w", err)
	}
	defer mb.Close()
	// 2. La richiesta si consuma sempre.
	requested, reqErr := mb.ReadRequestVersion()
	defer mb.Remove(FileApplyRequest)
	if reqErr != nil {
		log.Warn("apply.request illeggibile", "errore", reqErr.Error())
	}
	version := requested
	rejected := func(reason string, cause error) error {
		log.Error("aggiornamento rifiutato dall'updater", "versione", version, "motivo", reason, "errore", cause.Error())
		if werr := mb.WriteOutcome(FileApplyRejected, Outcome{Version: version, Reason: reason}); werr != nil {
			log.Error("apply.rejected non scritto", "errore", werr.Error())
		}
		return &Rejection{Reason: reason, Err: cause}
	}
	// 1. La cartella dell'updater, fuori dall'albero dell'utente.
	store, err := OpenStore(cfg.UpdaterDir, cfg.UID, cfg.TrustedBase)
	if err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("cartella dell'updater: %w", err))
	}
	defer store.Close()
	defer store.RemoveCopies()
	// Un aggiornamento precedente ancora in attesa del marcatore (punto 6 della revisione): non se
	// ne sovrappone un altro, decide prima il timer.
	if p, ok, perr := store.ReadPending(); perr != nil || ok {
		if perr != nil {
			return rejected(ReasonDownloadFailed, fmt.Errorf("pending.json illeggibile: %w", perr))
		}
		return rejected(ReasonDownloadFailed, fmt.Errorf("un aggiornamento alla %s è ancora in attesa del collegamento: il timer di ritorno deciderà prima", p.Version))
	}
	// 3. Il file dell'host: la scelta del padrone della macchina, letta da root, non dal connettore.
	hc, _, herr := hostconf.Load(cfg.ConfigFile)
	if herr != nil {
		return rejected(ReasonHostConfig, fmt.Errorf("file dell'host %s illeggibile: %w", cfg.ConfigFile, herr))
	}
	if !hc.AutoUpdate {
		return rejected(ReasonHostConfig, fmt.Errorf("auto_update = no in %s: gli aggiornamenti sono spenti su questo host", cfg.ConfigFile))
	}
	// 4. Le copie, senza fiducia.
	if _, err := copyEntry(mb.Dir, FileManifest, store.Dir, FileManifest, MaxManifestBytes, 0o600); err != nil {
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("manifest: %w", err))
	}
	if _, err := copyEntry(mb.Dir, FileSignature, store.Dir, FileSignature, MaxManifestBytes, 0o600); err != nil {
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("firma: %w", err))
	}
	binSHA, err := copyEntry(mb.Dir, FileBinary, store.Dir, FileBinary, MaxBinaryBytes, 0o700)
	if err != nil {
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("binario: %w", err))
	}
	// 5. La riverifica, sulla copia (letta dalla cartella dell'updater, che è nostra).
	manRaw, err := store.readSmall(FileManifest, MaxManifestBytes)
	if err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("manifest copiato: %w", err))
	}
	sigRaw, err := store.readSmall(FileSignature, MaxManifestBytes)
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
	// 6. L'installazione.
	log.Info("aggiornamento verificato dall'updater: installo", "versione", m.Version, "precedente", cfg.Running)
	binDir, _, err := OpenDir(filepath.Dir(cfg.Binary))
	if err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("cartella del binario: %w", err))
	}
	defer binDir.Close()
	binName := filepath.Base(cfg.Binary)
	if _, err := copyEntry(binDir, binName, binDir, binName+PrevSuffix, MaxBinaryBytes, 0o755); err != nil {
		return rejected(ReasonDownloadFailed, fmt.Errorf("copia del binario attuale in %s: %w", binName+PrevSuffix, err))
	}
	// La copia finale si verifica di nuovo: è il contenuto che finirà in /usr/local/bin.
	if err := installVerified(store.Dir, FileBinary, binDir, binName+NewSuffix, m.SHA256); err != nil {
		_ = binDir.Remove(binName + NewSuffix)
		_ = binDir.Remove(binName + PrevSuffix)
		return rejected(ReasonOf(err, ReasonDownloadFailed), fmt.Errorf("scrittura di %s: %w", binName+NewSuffix, err))
	}
	p := Pending{Version: m.Version, PreviousVersion: cfg.Running, At: protocol.Timestamp(cfg.Now())}
	if err := store.WritePending(p); err != nil {
		_ = binDir.Remove(binName + NewSuffix)
		_ = binDir.Remove(binName + PrevSuffix)
		return rejected(ReasonDownloadFailed, fmt.Errorf("pending.json: %w", err))
	}
	// Il timer di ritorno si arma PRIMA dello scambio: senza rete di sicurezza non si installa.
	if err := cfg.Systemctl("restart", RollbackTimer); err != nil {
		_ = binDir.Remove(binName + NewSuffix)
		_ = binDir.Remove(binName + PrevSuffix)
		_ = store.Remove(FilePending)
		return rejected(ReasonDownloadFailed, fmt.Errorf("timer di ritorno: %w", err))
	}
	if err := binDir.root.Rename(binName+NewSuffix, binName); err != nil {
		_ = binDir.Remove(binName + NewSuffix)
		_ = binDir.Remove(binName + PrevSuffix)
		_ = store.Remove(FilePending)
		_ = cfg.Systemctl("stop", RollbackTimer)
		return rejected(ReasonDownloadFailed, fmt.Errorf("rinomina su %s: %w", cfg.Binary, err))
	}
	if err := mb.WriteOutcome(FileApplied, p); err != nil {
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

// installVerified copia src (dalla cartella dell'updater) in dst (nella cartella del binario, root
// 0755) calcolando lo SHA-256 di ciò che SCRIVE e lo confronta con wantSHA: se non torna, il file
// scritto si toglie e il motivo è sha256-mismatch. È la verifica che conta: quella sul contenuto che
// finisce in /usr/local/bin, non su una lettura precedente (punto 1c della revisione).
func installVerified(src *Dir, srcName string, dst *Dir, dstName, wantSHA string) error {
	got, err := copyEntry(src, srcName, dst, dstName, MaxBinaryBytes, 0o755)
	if err != nil {
		return err
	}
	if got != wantSHA {
		_ = dst.Remove(dstName)
		return reject(ReasonSHA256Mismatch, "sha256 della copia finale %s, manifest %s", got, wantSHA)
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
// tutti i marcatori connected-* (anche quelli stantii) e ferma il timer; altrimenti rimette .prev al
// posto del binario (rename atomico, modo 0755), scrive rolled-back.json (versione tentata, motivo
// no-welcome) e riavvia il servizio. Tutto dai descrittori delle cartelle, mai dai percorsi.
func RollbackCheck(cfg ApplyConfig) (RollbackAction, error) {
	if err := cfg.defaults(); err != nil {
		return RollbackNothing, err
	}
	log := cfg.Logger
	store, err := OpenStore(cfg.UpdaterDir, cfg.UID, cfg.TrustedBase)
	if err != nil {
		return RollbackNothing, fmt.Errorf("cartella dell'updater: %w", err)
	}
	defer store.Close()
	p, ok, err := store.ReadPending()
	if err != nil {
		return RollbackNothing, err
	}
	if !ok {
		log.Info("nessun aggiornamento in attesa: niente da fare")
		return RollbackNothing, nil
	}
	mb, err := OpenMailboxUnder(cfg.StateDir, cfg.ServiceUID)
	if err != nil {
		return RollbackNothing, fmt.Errorf("cassetta delle lettere: %w", err)
	}
	defer mb.Close()
	binDir, _, err := OpenDir(filepath.Dir(cfg.Binary))
	if err != nil {
		return RollbackNothing, fmt.Errorf("cartella del binario: %w", err)
	}
	defer binDir.Close()
	binName := filepath.Base(cfg.Binary)
	action := DecideRollback(true, mb.ConnectedExists(p.Version))
	switch action {
	case RollbackDisarm:
		log.Info("il connettore nuovo è rimasto collegato: disarmo il ritorno", "versione", p.Version)
		if err := store.Remove(FilePending); err != nil {
			log.Warn("pending.json non rimosso", "errore", err.Error())
		}
		if err := binDir.Remove(binName + PrevSuffix); err != nil {
			log.Warn(".prev non rimosso", "errore", err.Error())
		}
		mb.RemoveConnectedMarkers()
		_ = cfg.Systemctl("stop", RollbackTimer)
	case RollbackRestore:
		log.Error("il connettore nuovo non si è collegato in tempo: ripristino la versione precedente", "tentata", p.Version, "precedente", p.PreviousVersion)
		if err := binDir.root.Rename(binName+PrevSuffix, binName); err != nil {
			return action, fmt.Errorf("ripristino di %s: %w", cfg.Binary, err)
		}
		if err := binDir.root.Chmod(binName, 0o755); err != nil {
			log.Error("permessi del binario ripristinato", "errore", err.Error())
		}
		if err := mb.WriteOutcome(FileRolledBack, Outcome{Version: p.Version, Reason: ReasonNoWelcome}); err != nil {
			log.Error("rolled-back.json non scritto", "errore", err.Error())
		}
		_ = mb.Remove(FileApplied)
		mb.RemoveConnectedMarkers()
		_ = store.Remove(FilePending)
		if err := cfg.Systemctl("restart", ServiceName); err != nil {
			return action, err
		}
	}
	return action, nil
}

// copyEntry copia src/srcName in dst/dstName per contenuto, dai descrittori delle due cartelle: il
// sorgente si apre senza seguire link e senza bloccarsi, deve essere un file regolare (fstat) entro
// max byte (oltre: too-large); la destinazione si libera (unlink) e si ricrea con O_EXCL, il modo si
// impone con fchmod. Restituisce lo SHA-256 esadecimale di ciò che ha scritto. Un sorgente assente,
// non regolare o illeggibile è download-failed: ciò che il connettore ha preparato non è utilizzabile.
func copyEntry(src *Dir, srcName string, dst *Dir, dstName string, max int64, mode fs.FileMode) (string, error) {
	if err := src.noSymlink(srcName); err != nil {
		return "", reject(ReasonDownloadFailed, "%s: %v", srcName, err)
	}
	f, err := src.root.OpenFile(srcName, openFlags, 0)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%s: %v", srcName, err)
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%s: %v", srcName, err)
	}
	if !fst.Mode().IsRegular() {
		return "", reject(ReasonDownloadFailed, "%s non è un file regolare (%s)", srcName, fst.Mode().Type())
	}
	if fst.Size() > max {
		return "", reject(ReasonTooLarge, "%s: %d byte, oltre %d", srcName, fst.Size(), max)
	}
	if err := dst.Remove(dstName); err != nil {
		return "", reject(ReasonDownloadFailed, "%s: %v", dstName, err)
	}
	out, err := dst.root.OpenFile(dstName, createFlags, mode)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%s: %v", dstName, err)
	}
	fail := func(reason, format string, a ...any) (string, error) {
		out.Close()
		_ = dst.Remove(dstName)
		return "", reject(reason, format, a...)
	}
	if err := out.Chmod(mode); err != nil {
		return fail(ReasonDownloadFailed, "modo di %s: %v", dstName, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, max+1))
	if err != nil {
		return fail(ReasonDownloadFailed, "copia di %s: %v", srcName, err)
	}
	if n > max {
		return fail(ReasonTooLarge, "%s oltre %d byte", srcName, max)
	}
	if err := out.Sync(); err != nil {
		return fail(ReasonDownloadFailed, "sync di %s: %v", dstName, err)
	}
	if err := out.Close(); err != nil {
		_ = dst.Remove(dstName)
		return "", reject(ReasonDownloadFailed, "chiusura di %s: %v", dstName, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
