package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// WorkPlaceholder è il segnaposto della cartella di lavoro: sostituito SOLO all'inizio di un argomento.
const WorkPlaceholder = "{work}"

// MaxFetchBytes è il tetto di scarico di un passo fetch (§1.9).
const MaxFetchBytes = 256 << 20

// RunConfig configura l'esecuzione dei passi.
type RunConfig struct {
	// WorkDir è la cartella di lavoro dell'installazione, state-dir/catalog/work/<installId>/: creata
	// 0700 all'inizio e rimossa alla fine, successo o no.
	WorkDir string
	// Home è la cartella dell'utente del connettore: cartella corrente dei passi run, così i percorsi
	// relativi di una ricetta sono relativi alla home (senza shell `~` non si espande).
	Home string
	// Env è l'ambiente dei passi run; nil = quello del processo.
	Env []string
	// HTTPClient scarica i passi fetch; nil = client con la verifica TLS di sistema e redirezioni
	// solo verso https.
	HTTPClient *http.Client
	// MaxFetch è il tetto di scarico; zero = MaxFetchBytes (i test lo abbassano).
	MaxFetch int64
	// TimeoutUnit moltiplica timeoutSeconds; zero = un secondo (i test lo accorciano).
	TimeoutUnit time.Duration
	// Progress riceve il progresso dopo ogni passo riuscito (step conta da 1).
	Progress func(step, of int, message string)
}

func (cfg *RunConfig) defaults() {
	if cfg.MaxFetch <= 0 {
		cfg.MaxFetch = MaxFetchBytes
	}
	if cfg.TimeoutUnit <= 0 {
		cfg.TimeoutUnit = time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = DefaultHTTPClient()
	}
}

// StepFailure è un passo non riuscito: motivo (checksum-mismatch, step-failed, timeout), indice da 0,
// dettaglio e coda dell'output.
type StepFailure struct {
	Reason    string
	StepIndex int
	Detail    string
	LogTail   string
}

func (f *StepFailure) Error() string {
	return fmt.Sprintf("passo %d: %s: %s", f.StepIndex, f.Reason, f.Detail)
}

// Run esegue i passi uno alla volta, in ordine. Il primo che non riesce ferma tutto: i successivi non
// partono. La cartella di lavoro si rimuove sempre alla fine.
func Run(ctx context.Context, r Recipe, cfg RunConfig) *StepFailure {
	cfg.defaults()
	if err := os.MkdirAll(filepath.Dir(cfg.WorkDir), 0o700); err != nil {
		return &StepFailure{Reason: protocol.ReasonStepFailed, Detail: detail("cartella di lavoro: " + err.Error())}
	}
	// Un residuo della stessa installazione (connettore caduto a metà) non si mescola alla nuova corsa.
	_ = os.RemoveAll(cfg.WorkDir)
	if err := os.Mkdir(cfg.WorkDir, 0o700); err != nil {
		return &StepFailure{Reason: protocol.ReasonStepFailed, Detail: detail("cartella di lavoro: " + err.Error())}
	}
	defer os.RemoveAll(cfg.WorkDir)
	for i, s := range r.Steps {
		timeout := time.Duration(s.TimeoutSeconds) * cfg.TimeoutUnit
		var f *StepFailure
		var msg string
		if s.Fetch != nil {
			f = fetch(ctx, cfg, *s.Fetch, timeout)
			msg = "scaricato " + s.Fetch.Dest
		} else {
			f = run(ctx, cfg, s.Run, timeout)
			msg = "eseguito " + filepath.Base(s.Run[0])
		}
		if f != nil {
			f.StepIndex = i
			return f
		}
		if cfg.Progress != nil {
			cfg.Progress(i+1, len(r.Steps), protocol.TruncateRunes(fmt.Sprintf("passo %d di %d: %s", i+1, len(r.Steps), msg), protocol.MaxCatalogProgressMessage))
		}
	}
	return nil
}

// ExpandArgs sostituisce {work} con la cartella di lavoro SOLO quando un argomento comincia col
// segnaposto; ogni altro carattere resta letterale (nessuna shell: `a|b` è un argomento).
func ExpandArgs(args []string, work string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if rest, ok := strings.CutPrefix(a, WorkPlaceholder); ok {
			a = work + rest
		}
		out[i] = a
	}
	return out
}

// tail è un buffer che tiene solo gli ultimi n byte scritti.
type tail struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.n:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return protocol.TailBytes(string(t.buf), protocol.MaxCatalogLogTail)
}

func run(ctx context.Context, cfg RunConfig, argv []string, timeout time.Duration) *StepFailure {
	args := ExpandArgs(argv, cfg.WorkDir)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Niente shell: il primo argomento è il programma, gli altri arrivano tal quali.
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = cfg.Home
	cmd.Env = cfg.Env
	// Un gruppo di processi proprio: oltre il tempo si uccide anche ciò che il passo ha generato
	// (npm lancia node, che lancia altro).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out := &tail{n: protocol.MaxCatalogLogTail}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &StepFailure{Reason: protocol.ReasonTimeout, Detail: fmt.Sprintf("oltre %s", timeout), LogTail: out.String()}
	default:
		return &StepFailure{Reason: protocol.ReasonStepFailed, Detail: detail(err.Error()), LogTail: out.String()}
	}
}

// DefaultHTTPClient è il client dei passi fetch: verifica TLS di sistema e redirezioni solo verso
// https (un artefatto versionato non scende mai a http).
func DefaultHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("redirezione fuori da https")
		}
		if len(via) >= 10 {
			return errors.New("troppe redirezioni")
		}
		return nil
	}}
}

// fetch scarica in un temporaneo della cartella di lavoro, con tetto e sha256, e solo se il checksum
// torna lo rinomina in dest (con il bit di esecuzione se richiesto). Su qualunque rifiuto il
// temporaneo si toglie: nessun file lasciato.
func fetch(ctx context.Context, cfg RunConfig, f Fetch, timeout time.Duration) *StepFailure {
	cfg.defaults()
	if !strings.HasPrefix(f.URL, "https://") {
		return &StepFailure{Reason: protocol.ReasonStepFailed, Detail: detail("url non https: " + f.URL)}
	}
	if !DestRE.MatchString(f.Dest) {
		return &StepFailure{Reason: protocol.ReasonStepFailed, Detail: detail("dest non è un nome di file nella cartella di lavoro: " + f.Dest)}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(reason, format string, a ...any) *StepFailure {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = protocol.ReasonTimeout
		}
		return &StepFailure{Reason: reason, Detail: detail(fmt.Sprintf(format, a...))}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return fail(protocol.ReasonStepFailed, "richiesta: %v", err)
	}
	req.Header.Set("User-Agent", "monsterterminal-connector-catalog")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return fail(protocol.ReasonStepFailed, "scarico: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(protocol.ReasonStepFailed, "scarico: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > cfg.MaxFetch {
		return fail(protocol.ReasonStepFailed, "artefatto di %d byte oltre il tetto di %d", resp.ContentLength, cfg.MaxFetch)
	}
	root, err := os.OpenRoot(cfg.WorkDir)
	if err != nil {
		return fail(protocol.ReasonStepFailed, "cartella di lavoro: %v", err)
	}
	defer root.Close()
	tmp := "." + f.Dest + ".part"
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(protocol.ReasonStepFailed, "file temporaneo: %v", err)
	}
	cleanup := func() { out.Close(); _ = root.Remove(tmp) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, cfg.MaxFetch+1))
	if err != nil {
		cleanup()
		return fail(protocol.ReasonStepFailed, "scarico interrotto dopo %d byte: %v", n, err)
	}
	if n > cfg.MaxFetch {
		cleanup()
		return fail(protocol.ReasonStepFailed, "artefatto oltre il tetto di %d byte", cfg.MaxFetch)
	}
	if err := out.Close(); err != nil {
		_ = root.Remove(tmp)
		return fail(protocol.ReasonStepFailed, "scrittura: %v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		_ = root.Remove(tmp)
		return &StepFailure{Reason: protocol.ReasonChecksumMismatch, Detail: detail(fmt.Sprintf("sha256 %s, atteso %s", got, f.SHA256))}
	}
	mode := os.FileMode(0o600)
	if f.Executable {
		mode = 0o700
	}
	if err := root.Chmod(tmp, mode); err != nil {
		_ = root.Remove(tmp)
		return fail(protocol.ReasonStepFailed, "permessi: %v", err)
	}
	if err := root.Rename(tmp, f.Dest); err != nil {
		_ = root.Remove(tmp)
		return fail(protocol.ReasonStepFailed, "rinomina: %v", err)
	}
	return nil
}
