// Package files riceve i file di DragMeHere sugli host del connettore, schemi 0.7.0 (ADR 0409 A1.5).
//
// Un file arriva dal backend a blocchi in sequenza (file.put) e finisce in una cartella ESISTENTE della
// home dell'utente di servizio. Le garanzie, tutte qui e non nel backend:
//   - il percorso segue una regola chiusa (protocol.IsHomePath) e si risolve sul disco cartella per
//     cartella con openat e O_NOFOLLOW a partire dalla home: un symlink lungo il cammino è PATH_INVALID,
//     quindi nessun link nella home porta fuori;
//   - si scrive in un temporaneo `.mt-<transferId>.part` nella stessa cartella, aperto con O_EXCL, modo
//     0644: mai bit di esecuzione, proprietario l'utente di servizio;
//   - all'ultimo blocco si verifica lo sha256 del file intero e si rinomina in modo atomico; senza
//     overwrite la rinomina è RENAME_NOREPLACE, così un file comparso nel frattempo non si sovrascrive;
//   - al più 4 trasferimenti vivi, 60 secondi d'inattività al massimo, e alla caduta della connessione
//     i trasferimenti aperti si annullano: in ogni caso il temporaneo si rimuove.
//
// Nessun messaggio esegue nulla: dir.list legge solo i nomi delle cartelle.
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
	"slices"
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
	dirPath   string // percorso relativo alla home, per la risposta
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
	case protocol.TypeDirList:
		m.handleDirList(env)
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

// errNotDir: un segmento non è una cartella o non esiste.
var errNotDir = errors.New("cartella inesistente")

// openDir apre la cartella di un percorso valido della home senza seguire symlink: ogni segmento con
// openat(O_DIRECTORY|O_NOFOLLOW) a partire dalla home. Un symlink è PATH_INVALID, una cartella che
// manca è DIR_NOT_FOUND.
func (m *Manager) openDir(rel string) (int, string, error) {
	fd, err := syscall.Open(m.cfg.Home, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, protocol.CodeDirNotFound, fmt.Errorf("home %s non apribile: %w", m.cfg.Home, err)
	}
	for _, seg := range protocol.PathSegments(rel) {
		next, err := syscall.Openat(fd, seg, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			// Con O_DIRECTORY|O_NOFOLLOW Linux risponde ENOTDIR (non ELOOP) anche per un symlink: si
			// guarda cosa c'è al suo posto, senza seguirlo.
			link := errors.Is(err, syscall.ELOOP) || isSymlinkAt(fd, seg)
			_ = syscall.Close(fd)
			if link {
				return -1, protocol.CodePathInvalid, fmt.Errorf("%s: un symlink lungo il percorso", rel)
			}
			if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
				return -1, protocol.CodeDirNotFound, fmt.Errorf("%s: %w", rel, errNotDir)
			}
			return -1, protocol.CodeDirNotFound, fmt.Errorf("%s: %w", rel, err)
		}
		_ = syscall.Close(fd)
		fd = next
	}
	return fd, "", nil
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
	dirFD, code, err := m.openDir(p.Path)
	if err != nil {
		m.reply(re, code, err.Error())
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
	dest := t.filename
	if t.dirPath != "" {
		dest = t.dirPath + "/" + t.filename
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

// handleDirList elenca le cartelle di una cartella della home: solo nomi, senza nascoste né symlink,
// ordinati, al più 500.
func (m *Manager) handleDirList(env protocol.Envelope) {
	d, err := protocol.DecodeDirList(env.Body)
	switch {
	case errors.Is(err, protocol.ErrPathInvalid):
		m.reply(env.ID, protocol.CodePathInvalid, "percorso fuori dalla regola")
		return
	case err != nil:
		m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
		return
	}
	fd, code, err := m.openDir(d.Path)
	if err != nil {
		m.reply(env.ID, code, err.Error())
		return
	}
	f := os.NewFile(uintptr(fd), d.Path)
	defer f.Close()
	// Solo i nomi: Readdirnames non fa stat per percorso (ReadDir, col tipo ignoto, farebbe lstat sul
	// nome relativo, cioè fuori dalla cartella aperta). Ogni candidato si prova con openat
	// O_DIRECTORY|O_NOFOLLOW sulla cartella aperta: un symlink a una cartella non è una cartella.
	names, err := f.Readdirnames(-1)
	if err != nil {
		m.reply(env.ID, protocol.CodeDirNotFound, "cartella non leggibile: "+err.Error())
		return
	}
	dirs := []string{}
	for _, name := range names {
		if !protocol.IsPathSegment(name) {
			continue
		}
		sub, err := syscall.Openat(fd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		_ = syscall.Close(sub)
		dirs = append(dirs, name)
	}
	slices.Sort(dirs)
	res := protocol.DirListResult{Path: d.Path, Dirs: dirs}
	if len(dirs) > protocol.MaxDirsListed {
		res.Dirs, res.Truncated = dirs[:protocol.MaxDirsListed], true
	}
	m.send(protocol.TypeDirListResult, env.ID, res)
}
