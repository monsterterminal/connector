package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
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

// Download scarica url in mb/binary passando da un file temporaneo e una rinomina dentro la cassetta
// (dal descrittore della cartella), con il tetto di MaxBinaryBytes (too-large: un byte oltre il tetto
// ferma tutto), timeout e verifica dello SHA-256 atteso (sha256-mismatch). Il file nasce 0600: non è
// eseguibile e non si esegue MAI qui. Sul rifiuto il temporaneo è rimosso.
func Download(ctx context.Context, client *http.Client, url, wantSHA256 string, mb *Mailbox, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultDownloadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return reject(ReasonDownloadFailed, "richiesta: %v", err)
	}
	req.Header.Set("User-Agent", "monsterterminal-connector-update")
	resp, err := client.Do(req)
	if err != nil {
		return reject(ReasonDownloadFailed, "%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reject(ReasonDownloadFailed, "HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > binaryLimit {
		return reject(ReasonTooLarge, "Content-Length %d oltre %d", resp.ContentLength, binaryLimit)
	}
	// Un residuo di un giro precedente (richiesta mai applicata) non deve restare accanto al nuovo.
	_ = mb.Remove(FileApplyRequest)
	const tmp = ".download"
	if err := mb.Remove(tmp); err != nil {
		return reject(ReasonDownloadFailed, "temporaneo: %v", err)
	}
	f, err := mb.root.OpenFile(tmp, createFlags, 0o600)
	if err != nil {
		return reject(ReasonDownloadFailed, "temporaneo: %v", err)
	}
	cleanup := func() { f.Close(); _ = mb.Remove(tmp) }
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return reject(ReasonDownloadFailed, "permessi: %v", err)
	}
	h := sha256.New()
	// Un byte in più del tetto: se arriva, il file è troppo grande.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, binaryLimit+1))
	if err != nil {
		cleanup()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return reject(ReasonDownloadFailed, "tempo scaduto dopo %d byte", n)
		}
		return reject(ReasonDownloadFailed, "lettura: %v", err)
	}
	if n > binaryLimit {
		cleanup()
		return reject(ReasonTooLarge, "oltre %d byte", binaryLimit)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return reject(ReasonDownloadFailed, "sync: %v", err)
	}
	if err := f.Close(); err != nil {
		_ = mb.Remove(tmp)
		return reject(ReasonDownloadFailed, "chiusura: %v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		_ = mb.Remove(tmp)
		return reject(ReasonSHA256Mismatch, "sha256 %s, atteso %s", got, wantSHA256)
	}
	if err := mb.root.Rename(tmp, FileBinary); err != nil {
		_ = mb.Remove(tmp)
		return reject(ReasonDownloadFailed, "rinomina: %v", err)
	}
	return nil
}
