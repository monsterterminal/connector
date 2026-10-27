// Test del ricevitore di DragMeHere su un filesystem vero (una home temporanea). Asse A: il file arriva
// intero e solo dove deve; asse B: codici e risposte di ADR 0409 A1.5 (file.put.result per blocco e
// finale, PATH_INVALID, FILE_EXISTS, FILE_TOO_LARGE, DISK_FULL, TRANSFER_LIMIT,
// TRANSFER_UNKNOWN, CHECKSUM_MISMATCH, PROTOCOL_ERROR; sempre sotto ~/dragmehere, creata 0755 se manca;
// temporaneo `.mt-<transferId>.part`, 0644).
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

type msg struct {
	typ, re string
	body    []byte
}

type fakeOut struct {
	mu   sync.Mutex
	msgs []msg
}

func (f *fakeOut) Control(typ, re string, body any) error {
	raw, _ := json.Marshal(body)
	f.mu.Lock()
	f.msgs = append(f.msgs, msg{typ, re, raw})
	f.mu.Unlock()
	return nil
}

func (f *fakeOut) get(re string) (msg, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if m.re == re {
			return m, true
		}
	}
	return msg{}, false
}

type rig struct {
	t    *testing.T
	home string
	m    *Manager
	out  *fakeOut
}

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	home := t.TempDir()
	cfg := Config{Home: home}
	if mod != nil {
		mod(&cfg)
	}
	r := &rig{t: t, home: home, m: New(cfg), out: &fakeOut{}}
	r.m.Attach(r.out)
	t.Cleanup(r.m.Detach)
	return r
}

func (r *rig) send(typ string, body any) string {
	r.t.Helper()
	raw, _ := json.Marshal(body)
	id := protocol.NewUUID()
	if !r.m.Handle(protocol.Envelope{Type: typ, ID: id, Body: raw}) {
		r.t.Fatalf("%s non gestito", typ)
	}
	return id
}

// code è il codice d'errore della risposta a re ("" se non è un errore).
func (r *rig) code(re string) string {
	m, ok := r.out.get(re)
	if !ok {
		r.t.Fatalf("nessuna risposta a %s", re)
	}
	if m.typ != protocol.TypeError {
		return ""
	}
	var e protocol.ErrorBody
	_ = json.Unmarshal(m.body, &e)
	return e.Code
}

func (r *rig) result(re string) protocol.FilePutResult {
	r.t.Helper()
	m, ok := r.out.get(re)
	if !ok || m.typ != protocol.TypeFilePutResult {
		r.t.Fatalf("file.put.result atteso, ricevuto %s %s", m.typ, m.body)
	}
	var res protocol.FilePutResult
	_ = json.Unmarshal(m.body, &res)
	return res
}

type upload struct {
	id, path, name string
	data           []byte
	chunk          int
	overwrite      bool
}

func (u upload) put(i int) protocol.FilePut {
	sum := sha256.Sum256(u.data)
	total := (len(u.data) + u.chunk - 1) / u.chunk
	end := min((i+1)*u.chunk, len(u.data))
	return protocol.FilePut{TransferID: u.id, Path: u.path, Filename: u.name, Size: int64(len(u.data)),
		SHA256: hex.EncodeToString(sum[:]), Overwrite: u.overwrite, ChunkIndex: i, TotalChunks: total, Data: u.data[i*u.chunk : end]}
}

func newUpload(path, name string, size, chunk int) upload {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return upload{id: protocol.NewUUID(), path: path, name: name, data: data, chunk: chunk}
}

// all manda tutti i blocchi e restituisce l'id dell'ultimo.
func (r *rig) all(u upload) string {
	r.t.Helper()
	total := (len(u.data) + u.chunk - 1) / u.chunk
	var id string
	for i := 0; i < total; i++ {
		id = r.send(protocol.TypeFilePut, u.put(i))
		if i < total-1 {
			if res := r.result(id); res.Done || res.ChunkIndex != i {
				r.t.Fatalf("blocco %d: %+v", i, res)
			}
		}
	}
	return id
}

// drop è il percorso assoluto di un file sotto ~/dragmehere.
func (r *rig) drop(rel ...string) string {
	return filepath.Join(append([]string{r.home, protocol.DropDir}, rel...)...)
}

// parts elenca i temporanei in ~/dragmehere/<dir>.
func (r *rig) parts(dir string) []string {
	entries, _ := os.ReadDir(filepath.Join(r.home, protocol.DropDir, dir))
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestFileInteroABlocchiInDragmehereCreataSeManca(t *testing.T) {
	r := newRig(t, nil)
	u := newUpload("progetti", "foto.png", 500_000, protocol.MaxChunkBytes)
	first := r.send(protocol.TypeFilePut, u.put(0))
	if res := r.result(first); res.Received != protocol.MaxChunkBytes || res.Done {
		t.Fatalf("primo blocco: %+v", res)
	}
	// ~/dragmehere e la sottocartella nascono col primo blocco, 0755.
	for _, d := range []string{r.drop(), r.drop("progetti")} {
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", d, st, err)
		}
	}
	// Durante il trasferimento c'è solo il temporaneo, col nome di lunghezza fissa.
	if p := r.parts("progetti"); len(p) != 1 || p[0] != ".mt-"+u.id+".part" {
		t.Fatalf("temporanei %v", p)
	}
	if _, err := os.Stat(r.drop("progetti", "foto.png")); err == nil {
		t.Fatal("il file finale esiste prima dell'ultimo blocco")
	}
	r.send(protocol.TypeFilePut, u.put(1))
	res := r.result(r.send(protocol.TypeFilePut, u.put(2)))
	sum := sha256.Sum256(u.data)
	if !res.Done || res.Path != "dragmehere/progetti/foto.png" || res.Size != 500_000 || res.SHA256 != hex.EncodeToString(sum[:]) || res.Received != 500_000 {
		t.Fatalf("esito finale %+v", res)
	}
	got, err := os.ReadFile(r.drop("progetti", "foto.png"))
	if err != nil || string(got) != string(u.data) {
		t.Fatalf("contenuto diverso: %v", err)
	}
	st, _ := os.Stat(r.drop("progetti", "foto.png"))
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("modo %v, atteso 0644 senza bit di esecuzione", st.Mode().Perm())
	}
	if p := r.parts("progetti"); len(p) != 0 || r.m.Alive() != 0 {
		t.Fatalf("restano temporanei %v o trasferimenti %d", p, r.m.Alive())
	}
	// Senza sottocartella: in ~/dragmehere, un blocco solo; la home resta intatta.
	u2 := newUpload("", "nota.txt", 10, 64)
	if res := r.result(r.all(u2)); !res.Done || res.Path != "dragmehere/nota.txt" {
		t.Fatalf("in ~/dragmehere: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(r.home, "nota.txt")); err == nil {
		t.Fatal("un file è finito nella home invece che in ~/dragmehere")
	}
}

func TestDragmehereSymlinkOFileNonSiSegue(t *testing.T) {
	outside := t.TempDir()
	// ~/dragmehere symlink verso fuori.
	r := newRig(t, nil)
	if err := os.Symlink(outside, filepath.Join(r.home, protocol.DropDir)); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"", "x"} {
		if code := r.code(r.send(protocol.TypeFilePut, newUpload(sub, "a.txt", 10, 64).put(0))); code != protocol.CodePathInvalid {
			t.Errorf("~/dragmehere symlink, sub %q: %q", sub, code)
		}
	}
	// ~/dragmehere è un file.
	r2 := newRig(t, nil)
	if err := os.WriteFile(filepath.Join(r2.home, protocol.DropDir), []byte("non sono una cartella"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r2.code(r2.send(protocol.TypeFilePut, newUpload("", "a.txt", 10, 64).put(0))); code != protocol.CodePathInvalid {
		t.Errorf("~/dragmehere file: %q", code)
	}
	// Sottocartella symlink dentro ~/dragmehere.
	r3 := newRig(t, nil)
	_ = os.Mkdir(r3.drop(), 0o755)
	if err := os.Symlink(outside, r3.drop("link")); err != nil {
		t.Fatal(err)
	}
	if code := r3.code(r3.send(protocol.TypeFilePut, newUpload("link", "a.txt", 10, 64).put(0))); code != protocol.CodePathInvalid {
		t.Errorf("sottocartella symlink: %q", code)
	}
	// Un symlink al posto del file finale conta come esistente e non si segue.
	if err := os.Symlink(filepath.Join(outside, "bersaglio"), r3.drop("f.txt")); err != nil {
		t.Fatal(err)
	}
	if code := r3.code(r3.send(protocol.TypeFilePut, newUpload("", "f.txt", 10, 64).put(0))); code != protocol.CodeFileExists {
		t.Errorf("symlink al posto del file: %q", code)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("un file è uscito dalla home attraverso un symlink: %v", entries)
	}
	// La regola: vuoto o UN segmento; niente risalite, assoluti, nascosti, sottocartelle annidate.
	for _, path := range []string{"..", "../etc", "/etc", ".ssh", "a/b", "a//b", "a/", " a"} {
		raw := map[string]any{"transferId": protocol.NewUUID(), "path": path, "filename": "x.txt", "size": 1,
			"sha256": strings.Repeat("0", 64), "chunkIndex": 0, "totalChunks": 1, "data": "eA=="}
		if code := r3.code(r3.send(protocol.TypeFilePut, raw)); code != protocol.CodePathInvalid {
			t.Errorf("percorso %q: %q", path, code)
		}
	}
	for _, name := range []string{".bashrc", "a/b", "..", ""} {
		raw := map[string]any{"transferId": protocol.NewUUID(), "path": "", "filename": name, "size": 1,
			"sha256": strings.Repeat("0", 64), "chunkIndex": 0, "totalChunks": 1, "data": "eA=="}
		if code := r3.code(r3.send(protocol.TypeFilePut, raw)); code != protocol.CodePathInvalid {
			t.Errorf("nome %q: %q", name, code)
		}
	}
}

func TestFileEsistenteOverwriteECorsa(t *testing.T) {
	r := newRig(t, nil)
	_ = os.Mkdir(r.drop(), 0o755)
	if err := os.WriteFile(r.drop("c.txt"), []byte("vecchio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.code(r.send(protocol.TypeFilePut, newUpload("", "c.txt", 10, 64).put(0))); code != protocol.CodeFileExists {
		t.Errorf("file esistente senza overwrite: %q", code)
	}
	u := newUpload("", "c.txt", 10, 64)
	u.overwrite = true
	if res := r.result(r.all(u)); !res.Done {
		t.Fatalf("overwrite: %+v", res)
	}
	if got, _ := os.ReadFile(r.drop("c.txt")); string(got) != string(u.data) {
		t.Fatal("overwrite non ha sostituito il file")
	}
	// Una cartella non si sovrascrive mai.
	_ = os.Mkdir(r.drop("cartella"), 0o755)
	u = newUpload("", "cartella", 10, 64)
	u.overwrite = true
	if code := r.code(r.send(protocol.TypeFilePut, u.put(0))); code != protocol.CodePathInvalid {
		t.Errorf("overwrite su una cartella: %q", code)
	}
	// Senza overwrite, un file comparso durante il trasferimento non si sovrascrive (RENAME_NOREPLACE).
	u = newUpload("", "corsa.txt", 200, 100)
	r.send(protocol.TypeFilePut, u.put(0))
	if err := os.WriteFile(r.drop("corsa.txt"), []byte("arrivato prima"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.code(r.send(protocol.TypeFilePut, u.put(1))); code != protocol.CodeFileExists {
		t.Errorf("file comparso nel frattempo: %q", code)
	}
	if got, _ := os.ReadFile(r.drop("corsa.txt")); string(got) != "arrivato prima" {
		t.Fatal("il file comparso nel frattempo è stato sovrascritto")
	}
	if p := r.parts(""); len(p) != 0 {
		t.Fatalf("temporanei rimasti %v", p)
	}
}

func TestSequenzaChecksumETetti(t *testing.T) {
	r := newRig(t, nil)
	// Fuori ordine: PROTOCOL_ERROR e trasferimento annullato.
	u := newUpload("", "a.bin", 300, 100)
	r.send(protocol.TypeFilePut, u.put(0))
	if code := r.code(r.send(protocol.TypeFilePut, u.put(2))); code != protocol.CodeProtocolError {
		t.Errorf("fuori ordine: %q", code)
	}
	if code := r.code(r.send(protocol.TypeFilePut, u.put(1))); code != protocol.CodeTransferUnknown {
		t.Errorf("dopo l'annullamento: %q", code)
	}
	// Checksum sbagliato all'ultimo blocco.
	u = newUpload("", "b.bin", 150, 100)
	r.send(protocol.TypeFilePut, u.put(0))
	p := u.put(1)
	p.SHA256 = strings.Repeat("a", 64)
	if code := r.code(r.send(protocol.TypeFilePut, p)); code != protocol.CodeProtocolError {
		t.Errorf("sha256 cambiato a metà: %q", code)
	}
	u = newUpload("", "c.bin", 150, 100)
	sum := sha256.Sum256([]byte("altro"))
	first := u.put(0)
	first.SHA256 = hex.EncodeToString(sum[:])
	r.send(protocol.TypeFilePut, first)
	last := u.put(1)
	last.SHA256 = first.SHA256
	if code := r.code(r.send(protocol.TypeFilePut, last)); code != protocol.CodeChecksumMismatch {
		t.Errorf("checksum: %q", code)
	}
	if _, err := os.Stat(r.drop("c.bin")); err == nil {
		t.Error("file con checksum sbagliato rinominato")
	}
	// Dimensione oltre il tetto.
	raw := map[string]any{"transferId": protocol.NewUUID(), "path": "", "filename": "g.bin", "size": protocol.MaxFileBytes + 1,
		"sha256": strings.Repeat("0", 64), "chunkIndex": 0, "totalChunks": 300, "data": "eA=="}
	if code := r.code(r.send(protocol.TypeFilePut, raw)); code != protocol.CodeFileTooLarge {
		t.Errorf("oltre 50 MiB: %q", code)
	}
	// Blocchi che superano la dimensione dichiarata.
	u = newUpload("", "d.bin", 100, 100)
	p = u.put(0)
	p.TotalChunks = 2
	r.send(protocol.TypeFilePut, p)
	p.ChunkIndex = 1
	if code := r.code(r.send(protocol.TypeFilePut, p)); code != protocol.CodeFileTooLarge {
		t.Errorf("oltre la dimensione dichiarata: %q", code)
	}
	// Blocco successivo di un trasferimento mai aperto.
	if code := r.code(r.send(protocol.TypeFilePut, newUpload("", "e.bin", 200, 100).put(1))); code != protocol.CodeTransferUnknown {
		t.Errorf("trasferimento sconosciuto: %q", code)
	}
	if ps := r.parts(""); len(ps) != 0 || r.m.Alive() != 0 {
		t.Fatalf("restano temporanei %v o trasferimenti %d", ps, r.m.Alive())
	}
}

func TestLimiteDiQuattroSpazioPienoEInattivita(t *testing.T) {
	r := newRig(t, func(c *Config) { c.IdleTimeout = 200 * time.Millisecond })
	var ups []upload
	for i := 0; i < protocol.MaxTransfersAlive; i++ {
		u := newUpload("", "f"+string(rune('a'+i))+".bin", 200, 100)
		ups = append(ups, u)
		if res := r.result(r.send(protocol.TypeFilePut, u.put(0))); res.Done {
			t.Fatal(res)
		}
	}
	if code := r.code(r.send(protocol.TypeFilePut, newUpload("", "quinto.bin", 200, 100).put(0))); code != protocol.CodeTransferLimit {
		t.Errorf("quinto trasferimento: %q", code)
	}
	// file.cancel libera un posto ed è idempotente.
	id := r.send(protocol.TypeFileCancel, protocol.TransferBody{TransferID: ups[0].id})
	if m, _ := r.out.get(id); m.typ != protocol.TypeFileCancelResult {
		t.Fatalf("cancel: %s", m.typ)
	}
	id = r.send(protocol.TypeFileCancel, protocol.TransferBody{TransferID: protocol.NewUUID()})
	if m, _ := r.out.get(id); m.typ != protocol.TypeFileCancelResult {
		t.Fatalf("cancel di uno sconosciuto: %s", m.typ)
	}
	if r.m.Alive() != 3 {
		t.Fatalf("vivi %d dopo il cancel", r.m.Alive())
	}
	// Inattività: tutto si annulla e i temporanei spariscono.
	deadline := time.Now().Add(5 * time.Second)
	for r.m.Alive() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.m.Alive() != 0 || len(r.parts("")) != 0 {
		t.Fatalf("dopo l'inattività: vivi %d, temporanei %v", r.m.Alive(), r.parts(""))
	}
	if code := r.code(r.send(protocol.TypeFilePut, ups[1].put(1))); code != protocol.CodeTransferUnknown {
		t.Errorf("blocco dopo la scadenza: %q", code)
	}
	// Spazio libero insufficiente: DISK_FULL prima di scrivere.
	full := newRig(t, func(c *Config) { c.statfs = func(int) (uint64, error) { return 50, nil } })
	if code := full.code(full.send(protocol.TypeFilePut, newUpload("", "big.bin", 100, 100).put(0))); code != protocol.CodeDiskFull {
		t.Errorf("disco pieno: %q", code)
	}
	if len(full.parts("")) != 0 {
		t.Error("temporaneo creato senza spazio")
	}
}

func TestCadutaDellaConnessioneAnnullaITrasferimenti(t *testing.T) {
	r := newRig(t, nil)
	u := newUpload("", "x.bin", 200, 100)
	r.send(protocol.TypeFilePut, u.put(0))
	r.m.Detach()
	if r.m.Alive() != 0 || len(r.parts("")) != 0 {
		t.Fatalf("dopo Detach: vivi %d, temporanei %v", r.m.Alive(), r.parts(""))
	}
}
