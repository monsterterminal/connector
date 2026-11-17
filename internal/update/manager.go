package update

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/signing"
)

// Out è la connessione vista dal gestore: messaggi di controllo verso il backend.
type Out interface {
	Control(typ, re string, body any) error
}

// Ritmi del gestore (ADR 0438 D4): update.check subito dopo welcome e poi ogni 6 ore con scarto
// casuale di ±30 minuti; la cassetta delle lettere si rilegge ogni 15 secondi per riportare un
// apply.rejected scritto dall'updater mentre il connettore è collegato.
const (
	DefaultCheckInterval   = 6 * time.Hour
	DefaultCheckJitter     = 30 * time.Minute
	DefaultMailboxInterval = 15 * time.Second
)

// Config configura il gestore lato connettore.
type Config struct {
	// Dir è <state>/update, la cassetta delle lettere (cartella dell'utente del connettore).
	Dir string
	// Version è la versione in esecuzione.
	Version string
	// HostAutoUpdate è auto_update del file locale dell'host (ADR 0409 D6): assoluto.
	HostAutoUpdate bool
	// HTTPClient scarica i binari; nil = client con la verifica TLS di sistema.
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
	// Key è la chiave pubblica con cui verificare i manifest; nil = mt-updates-2026 incorporata.
	Key ed25519.PublicKey
	// Platform è la piattaforma di chi verifica; vuota = quella del binario in esecuzione.
	Platform Platform
	// Ritmi; zero = default.
	CheckInterval, CheckJitter, MailboxInterval, DownloadTimeout time.Duration
}

// Manager è il gestore degli aggiornamenti lato connettore: chiede, verifica, scarica, prepara.
type Manager struct {
	cfg Config
	key ed25519.PublicKey

	mu          sync.Mutex
	out         Out
	backendAuto bool // interruttore del backend (welcome/config.update); assente = acceso
	busy        bool // un download è in corso
	stop        chan struct{}
	wg          sync.WaitGroup
}

// New crea il gestore. La cartella Dir viene creata (0700) se manca.
func New(cfg Config) (*Manager, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultCheckInterval
	}
	if cfg.CheckJitter < 0 {
		cfg.CheckJitter = 0
	} else if cfg.CheckJitter == 0 && cfg.CheckInterval == DefaultCheckInterval {
		cfg.CheckJitter = DefaultCheckJitter
	}
	if cfg.MailboxInterval <= 0 {
		cfg.MailboxInterval = DefaultMailboxInterval
	}
	if cfg.Platform == (Platform{}) {
		cfg.Platform = ThisPlatform(cfg.Version)
	}
	key := cfg.Key
	if key == nil {
		k, err := signing.PublicKey(signing.Updates)
		if err != nil {
			return nil, err
		}
		key = k
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, key: key, backendAuto: true}, nil
}

// SetBackendAutoUpdate applica l'interruttore ricevuto in welcome.config o config.update (ADR 0438
// D6): nil = acceso.
func (m *Manager) SetBackendAutoUpdate(v *bool) {
	m.mu.Lock()
	m.backendAuto = v == nil || *v
	m.mu.Unlock()
}

// Attach si chiama dopo welcome: riporta gli esiti lasciati nella cassetta delle lettere, invia
// update.check e avvia i timer della sessione.
func (m *Manager) Attach(out Out) {
	m.mu.Lock()
	if m.stop != nil {
		close(m.stop)
	}
	m.out = out
	stop := make(chan struct{})
	m.stop = stop
	m.mu.Unlock()
	m.wg.Add(1)
	go m.loop(stop)
}

// Detach si chiama alla fine della connessione: ferma i timer; un download in corso continua e il
// suo esito si riporta alla connessione successiva, dalla cassetta delle lettere o dal backend che
// ripropone.
func (m *Manager) Detach() {
	m.mu.Lock()
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
	m.out = nil
	m.mu.Unlock()
}

// Wait attende la fine dei lavori in sfondo (solo test).
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) loop(stop chan struct{}) {
	defer m.wg.Done()
	m.reportMailbox()
	m.sendCheck()
	mailbox := time.NewTicker(m.cfg.MailboxInterval)
	defer mailbox.Stop()
	check := time.NewTimer(m.nextCheck())
	defer check.Stop()
	for {
		select {
		case <-stop:
			return
		case <-mailbox.C:
			m.reportMailbox()
		case <-check.C:
			m.sendCheck()
			check.Reset(m.nextCheck())
		}
	}
}

// nextCheck è l'intervallo fino al prossimo update.check: 6 ore ± 30 minuti (D4).
func (m *Manager) nextCheck() time.Duration {
	d := m.cfg.CheckInterval
	if j := m.cfg.CheckJitter; j > 0 {
		d += time.Duration(rand.Int64N(int64(2*j))) - j
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

func (m *Manager) send(typ, re string, body any) {
	m.mu.Lock()
	out := m.out
	m.mu.Unlock()
	if out == nil {
		return
	}
	if err := out.Control(typ, re, body); err != nil {
		m.cfg.Logger.Warn("aggiornamenti: invio non riuscito", "tipo", typ, "errore", err.Error())
	}
}

func (m *Manager) sendCheck() {
	m.send(protocol.TypeUpdateCheck, "", struct{}{})
}

func (m *Manager) result(r protocol.UpdateResult) {
	if len(r.Reason) > protocol.MaxUpdateReasonLength {
		r.Reason = r.Reason[:protocol.MaxUpdateReasonLength]
	}
	m.cfg.Logger.Info("aggiornamento: esito", "versione", r.Version, "esito", r.Outcome, "motivo", r.Reason)
	m.send(protocol.TypeUpdateResult, "", r)
}

// reportMailbox legge gli esiti lasciati dall'updater e dal ritorno e li riporta (D3 punti 3 e 4).
func (m *Manager) reportMailbox() {
	rep := CollectStartup(m.cfg.Dir, m.cfg.Version)
	for _, w := range rep.Warnings {
		m.cfg.Logger.Warn("aggiornamenti: cassetta delle lettere", "avviso", w)
	}
	if rep.Connected != "" {
		m.cfg.Logger.Info("aggiornamento riuscito: marcatore di collegamento scritto", "versione", rep.Connected)
	}
	for _, r := range rep.Results {
		m.result(r)
	}
}

// Handle elabora update.available e update.none; gli altri tipi non sono suoi.
func (m *Manager) Handle(env protocol.Envelope) bool {
	switch env.Type {
	case protocol.TypeUpdateNone:
		if err := protocol.DecodeUpdateNone(env.Body); err != nil {
			m.reply(env.ID, err.Error())
			return true
		}
		m.cfg.Logger.Info("aggiornamenti: nessuna versione nuova per questo host")
	case protocol.TypeUpdateAvailable:
		u, err := protocol.DecodeUpdateAvailable(env.Body)
		if err != nil {
			m.reply(env.ID, err.Error())
			return true
		}
		m.offer(u)
	default:
		return false
	}
	return true
}

func (m *Manager) reply(re, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	m.send(protocol.TypeError, re, protocol.ErrorBody{Code: protocol.CodeMessageInvalid, Message: msg})
}

// offer tratta un update.available (D3 punto 1, D6): verifica, interruttori, download in sfondo.
func (m *Manager) offer(u protocol.UpdateAvailable) {
	version := u.Manifest.Version
	sig, err := protocol.DecodeSignature(u.Signature)
	if err != nil {
		m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonBadSignature})
		return
	}
	if err := Verify(u.Manifest, sig, m.key, m.cfg.Platform); err != nil {
		m.cfg.Logger.Warn("aggiornamento rifiutato", "versione", version, "errore", err.Error())
		m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonOf(err, ReasonBadSignature)})
		return
	}
	m.mu.Lock()
	backend := m.backendAuto
	busy := m.busy
	if !busy {
		m.busy = true
	}
	m.mu.Unlock()
	if skip := Decide(m.cfg.HostAutoUpdate, backend, u.Manifest.IsSecurity()); skip != "" {
		if !busy {
			m.setBusy(false)
		}
		m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateSkipped, Reason: skip})
		return
	}
	if busy {
		m.cfg.Logger.Info("aggiornamento già in corso: proposta ignorata", "versione", version)
		return
	}
	if _, err := os.Lstat(filepath.Join(m.cfg.Dir, FileApplyRequest)); err == nil {
		m.setBusy(false)
		m.cfg.Logger.Info("una richiesta di aggiornamento è già in attesa dell'updater: proposta ignorata", "versione", version)
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.setBusy(false)
		m.prepare(u.Manifest, sig)
	}()
}

func (m *Manager) setBusy(b bool) {
	m.mu.Lock()
	m.busy = b
	m.mu.Unlock()
}

// prepare scarica, verifica lo SHA-256 e lascia la richiesta nella cassetta delle lettere. Non
// esegue mai il binario: lo installa l'updater root, dopo la sua verifica.
func (m *Manager) prepare(man protocol.UpdateManifest, sig []byte) {
	version := man.Version
	m.cfg.Logger.Info("aggiornamento disponibile: scarico", "versione", version, "sicurezza", man.IsSecurity())
	RemoveDownload(m.cfg.Dir)
	if _, err := Download(context.Background(), m.cfg.HTTPClient, man.URL, man.SHA256, m.cfg.Dir, m.cfg.DownloadTimeout); err != nil {
		m.cfg.Logger.Warn("aggiornamento: download rifiutato", "versione", version, "errore", err.Error())
		m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonOf(err, ReasonDownloadFailed)})
		return
	}
	canon, err := Canonical(man)
	if err != nil {
		m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonBadSignature})
		return
	}
	if err := WriteRequest(m.cfg.Dir, canon, sig, version); err != nil {
		RemoveDownload(m.cfg.Dir)
		m.cfg.Logger.Warn("aggiornamento: richiesta non scritta", "versione", version, "errore", err.Error())
		m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonDownloadFailed})
		return
	}
	m.cfg.Logger.Info("aggiornamento preparato: l'updater lo verifica e lo installa", "versione", version, "cartella", m.cfg.Dir)
}
