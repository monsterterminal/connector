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
// connettore dopo welcome e a intervalli; e lo stato dell'updater nella SUA cartella, fuori
// dall'albero dell'utente (ADR 0438 D3, amendment A1: /var/lib/monsterterminal-updater, root 0700).
const (
	// FileApplyRejected: l'updater ha rifiutato ciò che ha trovato in update/ (versione e motivo).
	FileApplyRejected = "apply.rejected"
	// FileApplied: l'updater ha installato la versione nuova e riavviato il servizio (copia leggibile
	// di pending.json per il connettore, che non può leggere la cartella dell'updater).
	FileApplied = "applied.json"
	// FileRolledBack: il timer ha ripristinato la versione precedente (versione tentata e motivo).
	FileRolledBack = "rolled-back.json"
	// FilePending vive nella cartella dell'updater (root): un aggiornamento aspetta il marcatore.
	FilePending = "pending.json"
	// ConnectedPrefix + versione: il connettore nuovo è rimasto collegato per la finestra (welcome
	// più ConnectedAfter): è il marcatore che disarma il ritorno.
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

// openFlags sono i flag di ogni apertura in lettura dalle cartelle condivise: mai seguire l'ultimo
// componente, mai bloccarsi (una FIFO al posto di un file si apre e si scopre con fstat), mai
// ereditare il descrittore.
const openFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

// createFlags sono i flag di ogni creazione: il nome deve essere libero (chi lo occupa lo si toglie
// prima con unlink, che non segue i link), mai seguire, mai bloccarsi.
const createFlags = os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

// Dir è una cartella aperta una volta sola come os.Root: ogni operazione passa dal descrittore, mai
// più dal percorso, così chi può scrivere su un antenato non può scambiare la cartella sotto i nostri
// piedi fra un controllo e l'uso (rilievo della revisione di sicurezza, 2026-11-17).
type Dir struct {
	root *os.Root
	// Path è il percorso d'apertura, solo per i log.
	Path string
}

// OpenDir apre una cartella esistente come Root e ne restituisce anche il proprietario e il modo,
// letti con fstat sul descrittore (non dal percorso).
func OpenDir(path string) (*Dir, fs.FileInfo, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := statRoot(root)
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	return &Dir{root: root, Path: path}, st, nil
}

// OpenSub apre una sottocartella (un solo componente, mai un symlink) come Root e ne dà il fstat.
func (d *Dir) OpenSub(name string) (*Dir, fs.FileInfo, error) {
	f, err := d.root.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	f.Close()
	sub, err := d.root.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	st, err := statRoot(sub)
	if err != nil {
		sub.Close()
		return nil, nil, err
	}
	return &Dir{root: sub, Path: filepath.Join(d.Path, name)}, st, nil
}

func statRoot(r *os.Root) (fs.FileInfo, error) {
	f, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s non è una cartella", r.Name())
	}
	return st, nil
}

// Close chiude il descrittore.
func (d *Dir) Close() error { return d.root.Close() }

// UID è il proprietario di una voce, da fstat/Lstat.
func UID(st fs.FileInfo) int {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid)
	}
	return -1
}

// readSmall legge un file piccolo dalla cartella: aperto senza seguire né bloccarsi, deve essere un
// file regolare entro max byte.
func (d *Dir) readSmall(name string, max int64) ([]byte, error) {
	f, err := d.root.OpenFile(name, openFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: non è un file regolare (%s)", name, st.Mode().Type())
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s: %d byte, oltre %d", name, st.Size(), max)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// readJSON legge e decodifica; ok=false se il file non c'è.
func (d *Dir) readJSON(name string, v any, max int64) (ok bool, err error) {
	raw, err := d.readSmall(name, max)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("%s: %w", name, err)
	}
	return true, nil
}

// writeExclusive crea il file (il nome si libera prima con unlink, che non segue i link), impone il
// modo con fchmod (le unità girano con UMask=0077) e scrive.
func (d *Dir) writeExclusive(name string, data []byte, mode fs.FileMode) error {
	if err := d.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := d.root.OpenFile(name, createFlags, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeAtomic scrive passando da un temporaneo nella stessa cartella e una rinomina.
func (d *Dir) writeAtomic(name string, data []byte, mode fs.FileMode) error {
	tmp := ".tmp-" + name
	if err := d.writeExclusive(tmp, data, mode); err != nil {
		return err
	}
	if err := d.root.Rename(tmp, name); err != nil {
		_ = d.root.Remove(tmp)
		return err
	}
	return nil
}

// Remove toglie una voce (unlink: mai segue un link). Un file assente non è un errore.
func (d *Dir) Remove(name string) error {
	if err := d.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// IsRegular dice se name esiste ed è un file regolare (Lstat sul descrittore della cartella).
func (d *Dir) IsRegular(name string) bool {
	st, err := d.root.Lstat(name)
	return err == nil && st.Mode().IsRegular()
}

// Names elenca le voci della cartella.
func (d *Dir) Names() ([]string, error) {
	entries, err := fs.ReadDir(d.root.FS(), ".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// ---- la cassetta delle lettere <state>/update/ ----

// Mailbox è <state>/update/: del connettore, letta e scritta anche da root (updater e ritorno).
type Mailbox struct{ *Dir }

// OpenMailbox è l'apertura del connettore sulla PROPRIA cartella, creata (0700) se manca.
func OpenMailbox(path string) (*Mailbox, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	d, _, err := OpenDir(path)
	if err != nil {
		return nil, err
	}
	return &Mailbox{d}, nil
}

// OpenMailboxUnder è l'apertura di root: dal Root di <state> (il cui genitore è di root), la
// sottocartella update/ senza seguire link, e deve appartenere allo stesso utente di <state>, che
// non è root. Così root non scrive mai in una cartella che non sia quella dell'utente del connettore.
func OpenMailboxUnder(stateDir string) (*Mailbox, error) {
	state, stateInfo, err := OpenDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("cartella di stato %s: %w", stateDir, err)
	}
	defer state.Close()
	owner := UID(stateInfo)
	if owner == 0 {
		return nil, fmt.Errorf("%s appartiene a root: attesa la cartella dell'utente del connettore", stateDir)
	}
	sub, info, err := state.OpenSub(UpdateSubdir)
	if err != nil {
		return nil, fmt.Errorf("cassetta %s/%s: %w", stateDir, UpdateSubdir, err)
	}
	if UID(info) != owner {
		sub.Close()
		return nil, fmt.Errorf("%s/%s appartiene a uid %d, la cartella di stato a %d", stateDir, UpdateSubdir, UID(info), owner)
	}
	return &Mailbox{sub}, nil
}

// WriteOutcome scrive un esito (apply.rejected, applied.json, rolled-back.json) leggibile dal
// connettore (0644).
func (m *Mailbox) WriteOutcome(name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.writeExclusive(name, append(raw, '\n'), 0o644)
}

// ReadOutcome legge apply.rejected o rolled-back.json; ok=false se non c'è.
func (m *Mailbox) ReadOutcome(name string) (o Outcome, ok bool, err error) {
	ok, err = m.readJSON(name, &o, MaxManifestBytes)
	return o, ok, err
}

// ReadApplied legge applied.json; ok=false se non c'è.
func (m *Mailbox) ReadApplied() (p Pending, ok bool, err error) {
	ok, err = m.readJSON(FileApplied, &p, MaxManifestBytes)
	return p, ok, err
}

// ReadRequestVersion legge la versione scritta in apply.request (prima riga).
func (m *Mailbox) ReadRequestVersion() (string, error) {
	raw, err := m.readSmall(FileApplyRequest, 256)
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

// WriteRequest lascia manifest canonico, firma e apply.request (contenuto: la versione), in
// quest'ordine: l'unità path osserva apply.request e parte quando il resto è già sul disco.
func (m *Mailbox) WriteRequest(canonical, signature []byte, version string) error {
	for _, f := range []struct {
		name string
		data []byte
	}{{FileManifest, canonical}, {FileSignature, signature}, {FileApplyRequest, []byte(version + "\n")}} {
		if err := m.writeAtomic(f.name, f.data, 0o600); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

// RemoveDownload toglie ciò che il connettore aveva preparato (binario, manifest, firma, richiesta).
func (m *Mailbox) RemoveDownload() {
	for _, n := range []string{FileBinary, FileManifest, FileSignature, FileApplyRequest} {
		_ = m.Remove(n)
	}
}

// ConnectedMarker è il nome del marcatore della versione.
func ConnectedMarker(version string) string { return ConnectedPrefix + version }

// WriteConnected scrive il marcatore (file vuoto).
func (m *Mailbox) WriteConnected(version string) error {
	if !protocol.IsSemver(version) {
		return fmt.Errorf("versione %q non semver", version)
	}
	return m.writeExclusive(ConnectedMarker(version), nil, 0o600)
}

// ConnectedExists dice se c'è il marcatore della versione (solo un file regolare conta).
func (m *Mailbox) ConnectedExists(version string) bool { return m.IsRegular(ConnectedMarker(version)) }

// RemoveConnectedMarkers toglie tutti i marcatori connected-*, anche quelli stantii di versioni
// passate (punto 6 della revisione).
func (m *Mailbox) RemoveConnectedMarkers() {
	names, err := m.Names()
	if err != nil {
		return
	}
	for _, n := range names {
		if strings.HasPrefix(n, ConnectedPrefix) {
			_ = m.Remove(n)
		}
	}
}

// Item è un esito da riportare: il file si toglie SOLO dopo che il backend lo ha ricevuto (Ack),
// altrimenti si ripresenta al prossimo welcome (punto 3 della revisione: un rolled-back non deve
// potersi perdere, il fermo automatico di D5 ne dipende).
type Item struct {
	Result protocol.UpdateResult
	Ack    func() error
}

// Startup è ciò che il connettore trova nella cassetta dopo welcome.
type Startup struct {
	Items []Item
	// Applied è applied.json della versione in esecuzione: l'ok si manda dopo la finestra di
	// collegamento (ConnectedAfter), e solo allora si scrive il marcatore e si toglie il file.
	Applied *Pending
	// Warnings sono file illeggibili o incoerenti, da scrivere nel log.
	Warnings []string
}

// Collect legge gli esiti senza consumarli: apply.rejected diventa `failed` col suo motivo, oppure
// `skipped` se il motivo è un interruttore (host-config, tenant-switch); rolled-back.json diventa
// `rolled-back`; applied.json della versione in esecuzione è Applied; applied.json di un'altra
// versione è un residuo (per esempio dopo un ritorno) e si toglie subito. Un file illeggibile si
// toglie con un avviso: non c'è niente da riportare.
func (m *Mailbox) Collect(running string) Startup {
	var s Startup
	ack := func(names ...string) func() error {
		return func() error {
			for _, n := range names {
				if err := m.Remove(n); err != nil {
					return err
				}
			}
			m.RemoveDownload()
			return nil
		}
	}
	if o, ok, err := m.ReadOutcome(FileApplyRejected); ok {
		if err != nil {
			s.Warnings = append(s.Warnings, err.Error())
			_ = m.Remove(FileApplyRejected)
			m.RemoveDownload()
		} else {
			outcome := protocol.UpdateFailed
			if o.Reason == ReasonHostConfig || o.Reason == ReasonTenantSwitch {
				outcome = protocol.UpdateSkipped
			}
			s.Items = append(s.Items, Item{Result: protocol.UpdateResult{Version: o.Version, Outcome: outcome, Reason: o.Reason}, Ack: ack(FileApplyRejected)})
		}
	}
	if o, ok, err := m.ReadOutcome(FileRolledBack); ok {
		if err != nil {
			s.Warnings = append(s.Warnings, err.Error())
			_ = m.Remove(FileRolledBack)
			m.RemoveDownload()
		} else {
			s.Items = append(s.Items, Item{Result: protocol.UpdateResult{Version: o.Version, Outcome: protocol.UpdateRolledBack, Reason: o.Reason}, Ack: ack(FileRolledBack)})
		}
	}
	if p, ok, err := m.ReadApplied(); ok {
		switch {
		case err != nil:
			s.Warnings = append(s.Warnings, err.Error())
			_ = m.Remove(FileApplied)
			m.RemoveDownload()
		case p.Version == running:
			s.Applied = &p
		default:
			s.Warnings = append(s.Warnings, fmt.Sprintf("applied.json parla della %s ma gira la %s: residuo tolto", p.Version, running))
			_ = m.Remove(FileApplied)
			m.RemoveDownload()
		}
	}
	return s
}

// ---- la cartella dell'updater, fuori dall'albero dell'utente ----

// Store è la cartella dell'updater (root 0700): pending.json e le copie da verificare.
type Store struct{ *Dir }

// UpdaterDirFor è la cartella dell'updater per una cartella di stato: accanto, non sotto
// (/var/lib/monsterterminal → /var/lib/monsterterminal-updater; ADR 0438 A1).
func UpdaterDirFor(stateDir string) string { return filepath.Clean(stateDir) + "-updater" }

// OpenStore apre (creandola 0700 se manca) la cartella dell'updater. Prima controlla ogni antenato
// fra trustedBase (escluso) e la cartella: deve essere una cartella vera (non un symlink) di uid,
// non scrivibile da gruppo o altri; poi apre la cartella come Root e la verifica con fstat: cartella
// di uid, modo 0700. Un antenato in mano a qualcun altro renderebbe inutile ogni controllo sotto.
func OpenStore(path string, uid int, trustedBase string) (*Store, error) {
	path = filepath.Clean(path)
	if err := checkAncestors(path, uid, trustedBase); err != nil {
		return nil, err
	}
	switch st, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case st.Mode()&fs.ModeSymlink != 0:
		// OpenRoot seguirebbe un link sull'ultimo componente: qui no.
		return nil, fmt.Errorf("%s è un symlink", path)
	case !st.IsDir():
		return nil, fmt.Errorf("%s non è una cartella", path)
	}
	d, st, err := OpenDir(path)
	if err != nil {
		return nil, err
	}
	if UID(st) != uid {
		d.Close()
		return nil, fmt.Errorf("%s appartiene a uid %d, atteso %d", path, UID(st), uid)
	}
	if st.Mode().Perm() != 0o700 {
		d.Close()
		return nil, fmt.Errorf("%s ha permessi %o, attesi 0700", path, st.Mode().Perm())
	}
	return &Store{d}, nil
}

// checkAncestors controlla i genitori di path fino a trustedBase escluso.
func checkAncestors(path string, uid int, trustedBase string) error {
	trustedBase = filepath.Clean(trustedBase)
	for dir := filepath.Dir(path); dir != trustedBase && dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("antenato %s: %w", dir, err)
		}
		switch {
		case st.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("antenato %s è un symlink", dir)
		case !st.IsDir():
			return fmt.Errorf("antenato %s non è una cartella", dir)
		case UID(st) != uid:
			return fmt.Errorf("antenato %s appartiene a uid %d, atteso %d", dir, UID(st), uid)
		case st.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("antenato %s è scrivibile da altri (%o)", dir, st.Mode().Perm())
		}
	}
	return nil
}

// WritePending scrive pending.json (0600).
func (s *Store) WritePending(p Pending) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.writeAtomic(FilePending, raw, 0o600)
}

// ReadPending legge pending.json; ok=false se non esiste.
func (s *Store) ReadPending() (p Pending, ok bool, err error) {
	ok, err = s.readJSON(FilePending, &p, MaxManifestBytes)
	return p, ok, err
}

// RemoveCopies toglie le copie transitorie (binario, manifest, firma).
func (s *Store) RemoveCopies() {
	for _, n := range []string{FileBinary, FileManifest, FileSignature} {
		_ = s.Remove(n)
	}
}
