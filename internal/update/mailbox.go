package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Esiti lasciati dall'updater e dal ritorno nella cassetta delle lettere <state>/update/, letti dal
// connettore dopo welcome e a intervalli; e lo stato dell'updater in <state>/updater/ (root).
const (
	// FileApplyRejected: l'updater ha rifiutato ciò che ha trovato in update/ (versione e motivo).
	FileApplyRejected = "apply.rejected"
	// FileApplied: l'updater ha installato la versione nuova e riavviato il servizio (copia leggibile
	// di pending.json per il connettore, che non può leggere updater/).
	FileApplied = "applied.json"
	// FileRolledBack: il timer ha ripristinato la versione precedente (versione tentata e motivo).
	FileRolledBack = "rolled-back.json"
	// FilePending vive in updater/ (root): c'è un aggiornamento in attesa del marcatore di collegamento.
	FilePending = "pending.json"
	// ConnectedPrefix + versione: il connettore nuovo si è collegato (welcome ricevuto).
	ConnectedPrefix = "connected-"
)

// Pending è pending.json e applied.json: l'aggiornamento appena installato.
type Pending struct {
	Version         string `json:"version"`
	PreviousVersion string `json:"previousVersion"`
	At              string `json:"at"`
}

// Outcome è apply.rejected e rolled-back.json: un esito da riportare con update.result.
type Outcome struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// ConnectedMarker è il percorso del marcatore scritto dal connettore nuovo dopo welcome.
func ConnectedMarker(updateDir, version string) string {
	return filepath.Join(updateDir, ConnectedPrefix+version)
}

// WriteConnected scrive il marcatore (file vuoto) nella cartella del connettore.
func WriteConnected(updateDir, version string) error {
	if !protocol.IsSemver(version) {
		return fmt.Errorf("versione %q non semver", version)
	}
	return os.WriteFile(ConnectedMarker(updateDir, version), nil, 0o600)
}

// ConnectedExists dice se il connettore nuovo ha lasciato il marcatore (solo un file regolare conta).
func ConnectedExists(updateDir, version string) bool {
	st, err := os.Lstat(ConnectedMarker(updateDir, version))
	return err == nil && st.Mode().IsRegular()
}

// WritePending scrive pending.json nella cartella dell'updater (root, 0600).
func WritePending(updaterDir string, p Pending) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFile0600(filepath.Join(updaterDir, FilePending), raw)
}

// ReadPending legge pending.json; ok=false se non esiste.
func ReadPending(updaterDir string) (p Pending, ok bool, err error) {
	raw, err := readSmallNoFollow(filepath.Join(updaterDir, FilePending), MaxManifestBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, true, fmt.Errorf("pending.json: %w", err)
	}
	return p, true, nil
}

// WriteToMailbox scrive un file nella cartella del connettore da parte di root (apply.rejected,
// applied.json, rolled-back.json): la cartella è dell'utente non privilegiato, quindi mai seguire un
// symlink messo lì col nome del file. Si toglie il nome (unlink non segue i link) e si crea con
// O_EXCL|O_NOFOLLOW, leggibile dal connettore (0644).
func WriteToMailbox(updateDir, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(updateDir, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	// fchmod: l'unità dell'updater gira con UMask=0077 e il connettore deve poter leggere l'esito.
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadOutcome legge un esito (apply.rejected o rolled-back.json); ok=false se non c'è.
func ReadOutcome(updateDir, name string) (o Outcome, ok bool, err error) {
	raw, err := readSmallNoFollow(filepath.Join(updateDir, name), MaxManifestBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, true, fmt.Errorf("%s: %w", name, err)
	}
	return o, true, nil
}

// ReadApplied legge applied.json; ok=false se non c'è.
func ReadApplied(updateDir string) (p Pending, ok bool, err error) {
	raw, err := readSmallNoFollow(filepath.Join(updateDir, FileApplied), MaxManifestBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, true, fmt.Errorf("applied.json: %w", err)
	}
	return p, true, nil
}

// ReadRequestVersion legge la versione scritta in apply.request (prima riga, senza spazi).
func ReadRequestVersion(updateDir string) (string, error) {
	raw, err := readSmallNoFollow(filepath.Join(updateDir, FileApplyRequest), 256)
	if err != nil {
		return "", err
	}
	v, _, _ := strings.Cut(string(raw), "\n")
	v = strings.TrimSpace(v)
	if !protocol.IsSemver(v) {
		return "", fmt.Errorf("apply.request: %q non è una versione", v)
	}
	return v, nil
}

// readSmallNoFollow legge un file piccolo senza seguire symlink e solo se è un file regolare.
func readSmallNoFollow(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: non è un file regolare", filepath.Base(path))
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s: %d byte, oltre %d", filepath.Base(path), st.Size(), max)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// StartupReport è ciò che il connettore riporta al backend dopo welcome (D3 punti 3 e 4), nell'ordine
// in cui lo trova nella cassetta delle lettere: prima i rifiuti dell'updater, poi i ritorni indietro,
// poi l'installazione riuscita della propria versione.
type StartupReport struct {
	Results []protocol.UpdateResult
	// Connected è la versione per cui è stato scritto il marcatore (vuota se nessuna).
	Connected string
	// Warnings sono file illeggibili o incoerenti, da scrivere nel log.
	Warnings []string
}

// CollectStartup legge e CONSUMA gli esiti nella cassetta delle lettere: apply.rejected diventa
// `failed` col suo motivo, rolled-back.json diventa `rolled-back`, applied.json con la versione in
// esecuzione fa scrivere connected-<versione> e diventa `ok`; un applied.json di un'altra versione è
// un residuo (per esempio dopo un ritorno indietro) e si toglie senza riportare nulla.
func CollectStartup(updateDir, running string) StartupReport {
	var rep StartupReport
	if o, ok, err := ReadOutcome(updateDir, FileApplyRejected); ok {
		if err != nil {
			rep.Warnings = append(rep.Warnings, err.Error())
		} else {
			rep.Results = append(rep.Results, protocol.UpdateResult{Version: o.Version, Outcome: protocol.UpdateFailed, Reason: o.Reason})
		}
		_ = os.Remove(filepath.Join(updateDir, FileApplyRejected))
		RemoveDownload(updateDir)
	}
	if o, ok, err := ReadOutcome(updateDir, FileRolledBack); ok {
		if err != nil {
			rep.Warnings = append(rep.Warnings, err.Error())
		} else {
			rep.Results = append(rep.Results, protocol.UpdateResult{Version: o.Version, Outcome: protocol.UpdateRolledBack, Reason: o.Reason})
		}
		_ = os.Remove(filepath.Join(updateDir, FileRolledBack))
		RemoveDownload(updateDir)
	}
	if p, ok, err := ReadApplied(updateDir); ok {
		switch {
		case err != nil:
			rep.Warnings = append(rep.Warnings, err.Error())
		case p.Version == running:
			if err := WriteConnected(updateDir, running); err != nil {
				rep.Warnings = append(rep.Warnings, "marcatore di collegamento: "+err.Error())
			} else {
				rep.Connected = running
				rep.Results = append(rep.Results, protocol.UpdateResult{Version: running, Outcome: protocol.UpdateOK})
			}
		default:
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("applied.json parla della %s ma gira la %s: residuo tolto", p.Version, running))
		}
		_ = os.Remove(filepath.Join(updateDir, FileApplied))
		RemoveDownload(updateDir)
	}
	return rep
}
