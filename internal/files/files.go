// Package files riceve i file di DragMeHere sugli host del connettore, schemi 0.7.0 (ADR 0409 A1.5).
//
// Un file arriva dal backend a blocchi in sequenza (file.put) e finisce SEMPRE in ~/dragmehere, la
// cartella di DragMeHere nella home dell'utente di servizio, o in una sua sottocartella di un solo
// livello scritta dall'utente (decisione BOSS 2026-10-27: niente navigazione delle cartelle). Le
// garanzie, tutte qui e non nel backend:
//   - la sottocartella segue la regola dei nomi (protocol.IsDropSubdir); ~/dragmehere e la
//     sottocartella si aprono con openat e O_NOFOLLOW dalla home e si creano (0755) se mancano: se al
//     loro posto c'è un file o un symlink è PATH_INVALID, quindi nessun link porta fuori;
//   - si scrive in un temporaneo `.mt-<transferId>.part` nella stessa cartella, aperto con O_EXCL, modo
//     0644: mai bit di esecuzione, proprietario l'utente di servizio;
//   - all'ultimo blocco si verifica lo sha256 del file intero e si rinomina in modo atomico; senza
//     overwrite la rinomina è RENAME_NOREPLACE, così un file comparso nel frattempo non si sovrascrive;
//   - al più 4 trasferimenti vivi, 60 secondi d'inattività al massimo, e alla caduta della connessione
//     i trasferimenti aperti si annullano: in ogni caso il temporaneo si rimuove.
//
// Nessun messaggio esegue nulla.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Out è la connessione vista dal ricevitore: messaggi di controllo.
type Out interface {
	Control(typ, re string, body any) error
}

// Config configura il ricevitore.
type Config struct {
	// Home è la home dell'utente di servizio: la radice di ogni percorso.
	Home string
	// IdleTimeout è l'inattività massima di un trasferimento; zero = 60 secondi.
	IdleTimeout time.Duration
	Logger      *slog.Logger
	// statfs, se non nil, sostituisce la lettura dello spazio libero (per i test).
	statfs func(fd int) (free uint64, err error)
}

type transfer struct {
	id        string
	dirFD     int
	dirPath   string // sottocartella di ~/dragmehere, per la risposta
	filename  string
	part      string
	file      *os.File
	size      int64
	sha       string
	overwrite bool
	total     int
	next      int
	received  int64
	hash      hash.Hash
	timer     *time.Timer
}

// Manager tiene i trasferimenti della connessione corrente.
type Manager struct {
	cfg Config

	mu        sync.Mutex
	out       Out
	transfers map[string]*transfer
}

// New crea il ricevitore.
func New(cfg Config) *Manager {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.statfs == nil {
		cfg.statfs = func(fd int) (uint64, error) {
			var st syscall.Statfs_t
			if err := syscall.Fstatfs(fd, &st); err != nil {
				return 0, err
			}
			return st.Bavail * uint64(st.Bsize), nil
		}
	}
	return &Manager{cfg: cfg, transfers: map[string]*transfer{}}
}

// Attach collega il ricevitore a una connessione nuova.
func (m *Manager) Attach(out Out) {
	m.mu.Lock()
	m.out = out
	m.mu.Unlock()
}

// Detach annulla i trasferimenti aperti alla fine della connessione: il backend, che vede la
// connessione chiusa, ritenta da capo con un transferId nuovo.
func (m *Manager) Detach() {
	m.mu.Lock()
	ts := make([]*transfer, 0, len(m.transfers))
	for _, t := range m.transfers {
		ts = append(ts, t)
	}
	m.transfers = map[string]*transfer{}
	m.out = nil
	m.mu.Unlock()
	for _, t := range ts {
		m.discard(t)
	}
}

// Alive restituisce il numero di trasferimenti aperti (per i test e i log).
func (m *Manager) Alive() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.transfers)
}

// Handle elabora un messaggio di DragMeHere; false se il tipo non è suo.
func (m *Manager) Handle(env protocol.Envelope) bool {
	switch env.Type {
	case protocol.TypeFilePut:
		m.handlePut(env)
	case protocol.TypeFileCancel:
		c, err := protocol.DecodeFileCancel(env.Body)
		if err != nil {
			m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
			return true
		}
		if t := m.take(c.TransferID); t != nil {
			m.discard(t)
		}
		m.send(protocol.TypeFileCancelResult, env.ID, protocol.TransferBody{TransferID: c.TransferID})
	default:
		return false
	}
	return true
}

func (m *Manager) send(typ, re string, body any) {
	m.mu.Lock()
	out := m.out
	m.mu.Unlock()
	if out != nil {
		_ = out.Control(typ, re, body)
	}
}

func (m *Manager) reply(re, code, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	m.send(protocol.TypeError, re, protocol.ErrorBody{Code: code, Message: msg})
}

// take toglie un trasferimento dalla mappa (nil se sconosciuto).
func (m *Manager) take(id string) *transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.transfers[id]
	delete(m.transfers, id)
	return t
}

// discard chiude e rimuove il temporaneo di un trasferimento già tolto dalla mappa.
func (m *Manager) discard(t *transfer) {
	if t.timer != nil {
		t.timer.Stop()
	}
	if t.file != nil {
		_ = t.file.Close()
	}
	_ = syscall.Unlinkat(t.dirFD, t.part)
	_ = syscall.Close(t.dirFD)
}

// oPath è O_PATH (Linux, uguale su amd64 e arm64): il pacchetto syscall non lo espone.
const oPath = 0x200000

// isSymlinkAt dice se name, dentro la cartella dirFD, è un symlink: aperto con O_PATH|O_NOFOLLOW (che
// apre il link stesso) e letto con fstat.
func isSymlinkAt(dirFD int, name string) bool {
	fd, err := syscall.Openat(dirFD, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFLNK
}

// openDropDir apre ~/dragmehere o la sua sottocartella sub, creandole (0755) se mancano. Ogni livello si
// apre con openat(O_DIRECTORY|O_NOFOLLOW) dal livello sopra: se al posto di una cartella c'è un symlink o
// un file la risposta è PATH_INVALID, mai seguirlo.
func (m *Manager) openDropDir(sub string) (int, error) {
	fd, err := syscall.Open(m.cfg.Home, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("home %s non apribile: %w", m.cfg.Home, err)
	}
	segs := []string{protocol.DropDir}
	if sub != "" {
		segs = append(segs, sub)
	}
	for _, seg := range segs {
		next, err := openOrCreateDir(fd, seg)
		_ = syscall.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// openOrCreateDir apre la cartella name dentro dirFD senza seguire symlink, creandola se manca.
func openOrCreateDir(dirFD int, name string) (int, error) {
	const flags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	created := false
	for attempt := 0; attempt < 2; attempt++ {
		fd, err := syscall.Openat(dirFD, name, flags, 0)
		if err == nil {
			if created {
				// 0755 anche contro un'umask stretta del servizio.
				_ = syscall.Fchmod(fd, 0o755)
			}
			return fd, nil
		}
		if !errors.Is(err, syscall.ENOENT) {
			// Con O_DIRECTORY|O_NOFOLLOW Linux risponde ENOTDIR (o ELOOP) sia per un symlink sia per un
			// file: in entrambi i casi al suo posto non c'è una cartella vera.
			kind := "un file"
			if errors.Is(err, syscall.ELOOP) || isSymlinkAt(dirFD, name) {
				kind = "un symlink"
			}
			return -1, fmt.Errorf("%s: al posto della cartella c'è %s (%v)", name, kind, err)
		}
		switch err := syscall.Mkdirat(dirFD, name, 0o755); {
		case err == nil:
			created = true
		case !errors.Is(err, syscall.EEXIST):
			return -1, fmt.Errorf("%s: cartella non creata: %w", name, err)
		}
		// Creata ora (o da qualcun altro nel frattempo): si riapre con le stesse regole.
	}
	return -1, fmt.Errorf("%s: cartella non apribile", name)
}

func (m *Manager) handlePut(env protocol.Envelope) {
	p, err := protocol.DecodeFilePut(env.Body)
	switch {
	case errors.Is(err, protocol.ErrFileTooLarge):
		m.reply(env.ID, protocol.CodeFileTooLarge, err.Error())
		return
	case errors.Is(err, protocol.ErrPathInvalid):
		m.reply(env.ID, protocol.CodePathInvalid, "percorso o nome di file fuori dalla regola")
		return
	case err != nil:
		m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
		return
	}
	if p.ChunkIndex == 0 {
		m.open(env.ID, p)
		return
	}
	m.mu.Lock()
	t := m.transfers[p.TransferID]
	m.mu.Unlock()
	if t == nil {
		m.reply(env.ID, protocol.CodeTransferUnknown, "trasferimento sconosciuto o scaduto: "+p.TransferID)
		return
	}
	m.chunk(env.ID, t, p)
}

// open apre un trasferimento col primo blocco.
func (m *Manager) open(re string, p protocol.FilePut) {
	m.mu.Lock()
	_, dup := m.transfers[p.TransferID]
	full := len(m.transfers) >= protocol.MaxTransfersAlive
	m.mu.Unlock()
	if dup {
		if t := m.take(p.TransferID); t != nil {
			m.discard(t)
		}
		m.reply(re, protocol.CodeProtocolError, "primo blocco ripetuto: trasferimento annullato")
		return
	}
	if full {
		m.reply(re, protocol.CodeTransferLimit, fmt.Sprintf("già %d trasferimenti in corso su questo host", protocol.MaxTransfersAlive))
		return
	}
	if int64(p.TotalChunks) > p.Size || int64(p.TotalChunks)*protocol.MaxChunkBytes < p.Size {
		m.reply(re, protocol.CodeProtocolError, fmt.Sprintf("%d blocchi per %d byte", p.TotalChunks, p.Size))
		return
	}
	dirFD, err := m.openDropDir(p.Path)
	if err != nil {
		m.reply(re, protocol.CodePathInvalid, err.Error())
		return
	}
	fail := func(code, msg string) { _ = syscall.Close(dirFD); m.reply(re, code, msg) }
	// La destinazione: senza overwrite non deve esistere; con overwrite deve essere un file (non una
	// cartella). O_NOFOLLOW: un symlink al suo posto conta come esistente, e non si segue.
	if fd, err := syscall.Openat(dirFD, p.Filename, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil || errors.Is(err, syscall.ELOOP) {
		isDir := false
		if err == nil {
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				isDir = true
			}
			_ = syscall.Close(fd)
		}
		switch {
		case isDir:
			fail(protocol.CodePathInvalid, p.Filename+" è una cartella")
			return
		case !p.Overwrite:
			fail(protocol.CodeFileExists, "esiste già "+p.Filename)
			return
		}
	}
	if free, err := m.cfg.statfs(dirFD); err == nil && free < uint64(p.Size) {
		fail(protocol.CodeDiskFull, fmt.Sprintf("spazio libero %d byte, servono %d", free, p.Size))
		return
	}
	part := ".mt-" + p.TransferID + ".part"
	fd, err := syscall.Openat(dirFD, part, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		fail(protocol.CodePathInvalid, "file temporaneo non creato: "+err.Error())
		return
	}
	// Il modo si fissa anche contro l'umask: 0644, mai bit di esecuzione.
	_ = syscall.Fchmod(fd, 0o644)
	t := &transfer{id: p.TransferID, dirFD: dirFD, dirPath: p.Path, filename: p.Filename, part: part,
		file: os.NewFile(uintptr(fd), part), size: p.Size, sha: p.SHA256, overwrite: p.Overwrite,
		total: p.TotalChunks, hash: sha256.New()}
	m.mu.Lock()
	if len(m.transfers) >= protocol.MaxTransfersAlive {
		m.mu.Unlock()
		m.discard(t)
		m.reply(re, protocol.CodeTransferLimit, fmt.Sprintf("già %d trasferimenti in corso su questo host", protocol.MaxTransfersAlive))
		return
	}
	m.transfers[t.id] = t
	t.timer = time.AfterFunc(m.cfg.IdleTimeout, func() { m.expire(t) })
	m.mu.Unlock()
	m.chunk(re, t, p)
}

// expire annulla un trasferimento fermo da troppo: un blocco successivo riceverà TRANSFER_UNKNOWN.
func (m *Manager) expire(t *transfer) {
	m.mu.Lock()
	if m.transfers[t.id] != t {
		m.mu.Unlock()
		return
	}
	delete(m.transfers, t.id)
	m.mu.Unlock()
	m.cfg.Logger.Warn("trasferimento fermo, annullato", "transferId", t.id)
	m.discard(t)
}

// abort annulla un trasferimento in corso e risponde con l'errore.
func (m *Manager) abort(re string, t *transfer, code, msg string) {
	if m.take(t.id) == t {
		m.discard(t)
	}
	m.reply(re, code, msg)
}

// chunk scrive un blocco nella sequenza e, all'ultimo, verifica e rinomina.
func (m *Manager) chunk(re string, t *transfer, p protocol.FilePut) {
	switch {
	case p.ChunkIndex != t.next:
		m.abort(re, t, protocol.CodeProtocolError, fmt.Sprintf("blocco %d, atteso %d: trasferimento annullato", p.ChunkIndex, t.next))
		return
	case p.TotalChunks != t.total || p.Size != t.size || p.SHA256 != t.sha || p.Filename != t.filename ||
		p.Path != t.dirPath || p.Overwrite != t.overwrite:
		m.abort(re, t, protocol.CodeProtocolError, "un blocco cambia i dati del trasferimento: annullato")
		return
	case t.received+int64(len(p.Data)) > t.size:
		m.abort(re, t, protocol.CodeFileTooLarge, "i blocchi superano la dimensione dichiarata: annullato")
		return
	}
	t.timer.Reset(m.cfg.IdleTimeout)
	if _, err := t.file.Write(p.Data); err != nil {
		code := protocol.CodePathInvalid
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
			code = protocol.CodeDiskFull
		}
		m.abort(re, t, code, "scrittura non riuscita: "+err.Error())
		return
	}
	t.hash.Write(p.Data)
	t.received += int64(len(p.Data))
	t.next++
	if t.next < t.total {
		m.send(protocol.TypeFilePutResult, re, protocol.FilePutResult{TransferID: t.id, ChunkIndex: p.ChunkIndex, Received: t.received})
		return
	}
	// Ultimo blocco.
	if t.received != t.size {
		m.abort(re, t, protocol.CodeProtocolError, fmt.Sprintf("ricevuti %d byte, dichiarati %d: annullato", t.received, t.size))
		return
	}
	if got := hex.EncodeToString(t.hash.Sum(nil)); got != t.sha {
		m.abort(re, t, protocol.CodeChecksumMismatch, "sha256 del file diverso da quello dichiarato: annullato")
		return
	}
	if err := t.file.Sync(); err != nil {
		m.abort(re, t, protocol.CodeDiskFull, "scrittura su disco non riuscita: "+err.Error())
		return
	}
	var flags uint
	if !t.overwrite {
		flags = renameNoReplace
	}
	if err := renameat2(t.dirFD, t.part, t.dirFD, t.filename, flags); err != nil {
		switch {
		case errors.Is(err, syscall.EEXIST):
			m.abort(re, t, protocol.CodeFileExists, "nel frattempo è comparso "+t.filename)
		case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOTEMPTY):
			m.abort(re, t, protocol.CodePathInvalid, t.filename+" è una cartella")
		default:
			m.abort(re, t, protocol.CodePathInvalid, "rinomina non riuscita: "+err.Error())
		}
		return
	}
	if m.take(t.id) == t {
		t.timer.Stop()
		_ = t.file.Close()
		_ = syscall.Close(t.dirFD)
	}
	dest := protocol.DropDir + "/" + t.filename
	if t.dirPath != "" {
		dest = protocol.DropDir + "/" + t.dirPath + "/" + t.filename
	}
	m.send(protocol.TypeFilePutResult, re, protocol.FilePutResult{TransferID: t.id, ChunkIndex: p.ChunkIndex,
		Received: t.received, Done: true, Path: dest, Size: t.size, SHA256: t.sha})
}

const renameNoReplace = 1 // RENAME_NOREPLACE

// renameat2 rinomina in modo atomico; con RENAME_NOREPLACE fallisce con EEXIST se la destinazione c'è.
func renameat2(olddirfd int, oldpath string, newdirfd int, newpath string, flags uint) error {
	op, err := syscall.BytePtrFromString(oldpath)
	if err != nil {
		return err
	}
	np, err := syscall.BytePtrFromString(newpath)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(sysRenameat2, uintptr(olddirfd), uintptr(unsafe.Pointer(op)),
		uintptr(newdirfd), uintptr(unsafe.Pointer(np)), uintptr(flags), 0)
	if e != 0 {
		return e
	}
	return nil
}
