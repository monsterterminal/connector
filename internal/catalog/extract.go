package catalog

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Tetti di `catalog extract`: byte scritti in totale e numero di voci. Un archivio oltre i tetti si
// ferma con un errore (una bomba di decompressione non riempie il disco dell'host).
const (
	MaxExtractBytes   = 1 << 30
	MaxExtractEntries = 100000
)

// ExtractLimits sono i tetti di Extract; zero = i default.
type ExtractLimits struct {
	MaxBytes   int64
	MaxEntries int
}

// ErrUnsafeEntry è una voce che uscirebbe dalla cartella di destinazione (percorso assoluto, `..`,
// symlink che punta fuori, collegamento fisico, dispositivo).
var ErrUnsafeEntry = errors.New("voce dell'archivio non sicura")

// Extract apre un archivio tar.gz o zip (riconosciuto dai primi byte, non dal nome) nella cartella
// dest, creata se manca, SENZA shell. Ogni scrittura passa da os.Root aperto su dest: nessuna voce
// può uscirne, nemmeno attraverso un symlink creato da una voce precedente. Le voci che uscirebbero
// fermano l'estrazione con ErrUnsafeEntry. Riestrarre lo stesso archivio dà lo stesso risultato: i
// file esistenti si sostituiscono.
func Extract(archive, dest string, lim ExtractLimits) error {
	if lim.MaxBytes <= 0 {
		lim.MaxBytes = MaxExtractBytes
	}
	if lim.MaxEntries <= 0 {
		lim.MaxEntries = MaxExtractEntries
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	x := &extractor{root: root, lim: lim}
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return x.tarGz(f)
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		return x.zip(f, fi.Size())
	}
	return errors.New("archivio non riconosciuto: attesi tar.gz o zip")
}

type extractor struct {
	root    *os.Root
	lim     ExtractLimits
	written int64
	entries int
}

// clean porta il nome di una voce a un percorso relativo pulito, o lo rifiuta.
func clean(name string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsRune(n, 0) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeEntry, name)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q contiene ..", ErrUnsafeEntry, name)
		}
	}
	c := path.Clean(n)
	if c == "." {
		return "", nil // la radice dell'archivio
	}
	return c, nil
}

// linkInside dice se un symlink in name verso target resta dentro la cartella.
func linkInside(name, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") {
		return false
	}
	j := path.Clean(path.Join(path.Dir(name), target))
	return j != ".." && !strings.HasPrefix(j, "../")
}

func (x *extractor) count() error {
	x.entries++
	if x.entries > x.lim.MaxEntries {
		return fmt.Errorf("archivio oltre %d voci", x.lim.MaxEntries)
	}
	return nil
}

// replace toglie ciò che c'è al posto di name (file o symlink; una cartella resta), per riestrarre.
func (x *extractor) replace(name string) error {
	fi, err := x.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%q: al suo posto c'è una cartella", name)
	}
	return x.root.Remove(name)
}

func (x *extractor) mkParent(name string) error {
	if d := path.Dir(name); d != "." {
		return x.root.MkdirAll(d, 0o755)
	}
	return nil
}

func (x *extractor) file(name string, mode fs.FileMode, r io.Reader) error {
	if err := x.mkParent(name); err != nil {
		return err
	}
	if err := x.replace(name); err != nil {
		return err
	}
	out, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	room := x.lim.MaxBytes - x.written
	n, err := io.Copy(out, io.LimitReader(r, room+1))
	x.written += n
	if err == nil && n > room {
		err = fmt.Errorf("archivio oltre %d byte estratti", x.lim.MaxBytes)
	}
	if err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Solo i permessi rwx: mai setuid, setgid o sticky da un archivio.
	return x.root.Chmod(name, mode.Perm()&0o755|0o600)
}

func (x *extractor) dir(name string) error {
	if name == "" {
		return nil
	}
	return x.root.MkdirAll(name, 0o755)
}

func (x *extractor) symlink(name, target string) error {
	if !linkInside(name, target) {
		return fmt.Errorf("%w: symlink %q → %q esce dalla cartella", ErrUnsafeEntry, name, target)
	}
	if err := x.mkParent(name); err != nil {
		return err
	}
	if err := x.replace(name); err != nil {
		return err
	}
	return x.root.Symlink(target, name)
}

func (x *extractor) tarGz(r io.Reader) error {
	gz, err := gzip.NewReader(bufio.NewReader(r))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := x.count(); err != nil {
			return err
		}
		name, err := clean(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = x.dir(name)
		case tar.TypeReg:
			if name == "" {
				return fmt.Errorf("%w: file senza nome", ErrUnsafeEntry)
			}
			err = x.file(name, fs.FileMode(h.Mode), tr)
		case tar.TypeSymlink:
			err = x.symlink(name, h.Linkname)
		case tar.TypeXGlobalHeader:
			// Metadati pax dell'archivio: niente da scrivere.
		default:
			return fmt.Errorf("%w: %q di tipo %q (ammessi file, cartelle e symlink interni)", ErrUnsafeEntry, h.Name, string(h.Typeflag))
		}
		if err != nil {
			return err
		}
	}
}

func (x *extractor) zip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	for _, zf := range zr.File {
		if err := x.count(); err != nil {
			return err
		}
		name, err := clean(zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			err = x.dir(name)
		case mode&fs.ModeSymlink != 0:
			rc, oerr := zf.Open()
			if oerr != nil {
				return oerr
			}
			target, rerr := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if rerr != nil {
				return rerr
			}
			err = x.symlink(name, string(target))
		case mode.IsRegular():
			if name == "" {
				return fmt.Errorf("%w: file senza nome", ErrUnsafeEntry)
			}
			rc, oerr := zf.Open()
			if oerr != nil {
				return oerr
			}
			err = x.file(name, mode, rc)
			rc.Close()
		default:
			return fmt.Errorf("%w: %q di tipo %v", ErrUnsafeEntry, zf.Name, mode.Type())
		}
		if err != nil {
			return err
		}
	}
	return nil
}
