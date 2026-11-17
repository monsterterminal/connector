package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Nomi dei file nella cassetta delle lettere <state>/update/ (scritti dal connettore).
const (
	FileBinary       = "binary"
	FileManifest     = "manifest.json"
	FileSignature    = "manifest.sig"
	FileApplyRequest = "apply.request"
)

// DefaultDownloadTimeout è il tempo massimo di un download: 10 minuti.
const DefaultDownloadTimeout = 10 * time.Minute

// binaryLimit è il tetto applicato da Download: MaxBinaryBytes, abbassato solo nei test.
var binaryLimit int64 = MaxBinaryBytes

// Download scarica url in dir/binary passando da un file temporaneo e una rinomina, con il tetto di
// MaxBinaryBytes (too-large: un byte oltre il tetto ferma tutto), timeout e verifica dello SHA-256
// atteso (sha256-mismatch). Il file nasce 0600: non è eseguibile e non si esegue MAI qui. Sul
// rifiuto il temporaneo è rimosso; sul successo restituisce il percorso del file.
func Download(ctx context.Context, client *http.Client, url, wantSHA256, dir string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultDownloadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "richiesta: %v", err)
	}
	req.Header.Set("User-Agent", "monsterterminal-connector-update")
	resp, err := client.Do(req)
	if err != nil {
		return "", reject(ReasonDownloadFailed, "%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", reject(ReasonDownloadFailed, "HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > binaryLimit {
		return "", reject(ReasonTooLarge, "Content-Length %d oltre %d", resp.ContentLength, binaryLimit)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", reject(ReasonDownloadFailed, "cartella: %v", err)
	}
	// Un residuo di un giro precedente (richiesta mai applicata) non deve restare accanto al nuovo.
	_ = os.Remove(filepath.Join(dir, FileApplyRequest))
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", reject(ReasonDownloadFailed, "temporaneo: %v", err)
	}
	name := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return "", reject(ReasonDownloadFailed, "permessi: %v", err)
	}
	h := sha256.New()
	// Un byte in più del tetto: se arriva, il file è troppo grande.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, binaryLimit+1))
	if err != nil {
		cleanup()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", reject(ReasonDownloadFailed, "tempo scaduto dopo %d byte", n)
		}
		return "", reject(ReasonDownloadFailed, "lettura: %v", err)
	}
	if n > binaryLimit {
		cleanup()
		return "", reject(ReasonTooLarge, "oltre %d byte", binaryLimit)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", reject(ReasonDownloadFailed, "sync: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", reject(ReasonDownloadFailed, "chiusura: %v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		os.Remove(name)
		return "", reject(ReasonSHA256Mismatch, "sha256 %s, atteso %s", got, wantSHA256)
	}
	dst := filepath.Join(dir, FileBinary)
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return "", reject(ReasonDownloadFailed, "rinomina: %v", err)
	}
	return dst, nil
}

// WriteRequest lascia nella cassetta delle lettere il manifest canonico, la firma e apply.request
// (contenuto: la versione), nell'ordine: l'unità updater osserva apply.request e parte solo quando
// tutto il resto è già sul disco. Il binario è già in dir/binary (Download).
func WriteRequest(dir string, canonical, signature []byte, version string) error {
	for _, f := range []struct {
		name string
		data []byte
	}{{FileManifest, canonical}, {FileSignature, signature}, {FileApplyRequest, []byte(version + "\n")}} {
		if err := writeFile0600(filepath.Join(dir, f.name), f.data); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

// writeFile0600 scrive per intero passando da un temporaneo e una rinomina.
func writeFile0600(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// RemoveDownload toglie dalla cassetta delle lettere ciò che il connettore aveva preparato (binario,
// manifest, firma, richiesta): dopo un esito riportato, o prima di un nuovo giro.
func RemoveDownload(dir string) {
	for _, n := range []string{FileBinary, FileManifest, FileSignature, FileApplyRequest} {
		_ = os.Remove(filepath.Join(dir, n))
	}
}
