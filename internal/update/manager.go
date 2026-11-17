package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
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
// apply.rejected scritto dall'updater mentre il connettore è collegato; il marcatore di collegamento
// si scrive solo dopo welcome E 30 secondi di connessione mantenuta (dentro la finestra dei 5 minuti
// del ritorno): un binario che si collega e cade subito non lo scrive (punto 7 della revisione).
const (
	DefaultCheckInterval   = 6 * time.Hour
	DefaultCheckJitter     = 30 * time.Minute
	DefaultMailboxInterval = 15 * time.Second
	DefaultConnectedAfter  = 30 * time.Second
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
	CheckInterval, CheckJitter, MailboxInterval, DownloadTimeout, ConnectedAfter time.Duration
}

// Manager è il gestore degli aggiornamenti lato connettore: chiede, verifica, scarica, prepara.
type Manager struct {
	cfg Config
	key ed25519.PublicKey
	mb  *Mailbox

	mu          sync.Mutex
	out         Out
	backendAuto bool // interruttore del backend (welcome/config.update); assente = acceso
	busy        bool // un download è in corso
	stop        chan struct{}
	loops       sync.WaitGroup // il ciclo della sessione (Attach/Detach)
	downloads   sync.WaitGroup // i download in sfondo
}

// New crea il gestore e apre la cassetta delle lettere (creata 0700 se manca) come os.Root: da qui in
// poi ogni accesso passa dal descrittore.
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
	if cfg.ConnectedAfter <= 0 {
		cfg.ConnectedAfter = DefaultConnectedAfter
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
	mb, err := OpenMailbox(cfg.Dir)
	if err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, key: key, mb: mb, backendAuto: true}, nil
}

// Close chiude la cassetta (fine del processo).
func (m *Manager) Close() error {
	m.Detach()
	return m.mb.Close()
}

// SetBackendAutoUpdate applica l'interruttore ricevuto in welcome.config o config.update (ADR 0438
// D6): nil = acceso.
func (m *Manager) SetBackendAutoUpdate(v *bool) {
	m.mu.Lock()
	m.backendAuto = v == nil || *v
	m.mu.Unlock()
}

// Attach si chiama dopo welcome: ferma e ASPETTA il ciclo della sessione precedente (così due
// riletture della cassetta non riportano due volte lo stesso esito), poi riporta gli esiti, invia
// update.check e avvia i timer della sessione.
func (m *Manager) Attach(out Out) {
	m.stopLoop()
	m.mu.Lock()
	m.out = out
	stop := make(chan struct{})
	m.stop = stop
	m.mu.Unlock()
	m.loops.Add(1)
	go m.loop(stop)
}

// Detach si chiama alla fine della connessione: ferma il ciclo e lo aspetta; un download in corso
// continua e il suo esito si riporta alla connessione successiva, dalla cassetta o dal backend che
// ripropone.
func (m *Manager) Detach() { m.stopLoop() }

func (m *Manager) stopLoop() {
	m.mu.Lock()
	old := m.stop
	m.stop = nil
	m.out = nil
	m.mu.Unlock()
	if old != nil {
		close(old)
		m.loops.Wait()
	}
}

// Wait attende la fine dei download in sfondo (solo test).
func (m *Manager) Wait() { m.downloads.Wait() }

func (m *Manager) loop(stop chan struct{}) {
	defer m.loops.Done()
	applied := m.reportMailbox()
	m.sendCheck()
	mailbox := time.NewTicker(m.cfg.MailboxInterval)
	defer mailbox.Stop()
	check := time.NewTimer(m.nextCheck())
	defer check.Stop()
	// La finestra di collegamento: solo se questa versione è appena stata installata.
	var connected <-chan time.Time
	if applied != nil {
		t := time.NewTimer(m.cfg.ConnectedAfter)
		defer t.Stop()
		connected = t.C
	}
	for {
		select {
		case <-stop:
			return
		case <-mailbox.C:
			m.reportMailbox()
		case <-check.C:
			m.sendCheck()
			check.Reset(m.nextCheck())
		case <-connected:
			connected = nil
			m.confirmConnected()
		}
	}
}

// confirmConnected: welcome ricevuto e connessione mantenuta per la finestra. Scrive il marcatore che
// disarma il ritorno e manda update.result ok; applied.json si toglie solo se l'invio è riuscito,
// altrimenti si ripresenta al prossimo welcome.
func (m *Manager) confirmConnected() {
	if err := m.mb.WriteConnected(m.cfg.Version); err != nil {
		m.cfg.Logger.Error("marcatore di collegamento non scritto", "errore", err.Error())
		return
	}
	m.cfg.Logger.Info("aggiornamento riuscito: connessione mantenuta, marcatore di collegamento scritto", "versione", m.cfg.Version)
	if err := m.result(protocol.UpdateResult{Version: m.cfg.Version, Outcome: protocol.UpdateOK}); err != nil {
		return
	}
	_ = m.mb.Remove(FileApplied)
	m.mb.RemoveDownload()
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

var errNotAttached = errors.New("non collegato")

func (m *Manager) send(typ, re string, body any) error {
	m.mu.Lock()
	out := m.out
	m.mu.Unlock()
	if out == nil {
		return errNotAttached
	}
	if err := out.Control(typ, re, body); err != nil {
		m.cfg.Logger.Warn("aggiornamenti: invio non riuscito", "tipo", typ, "errore", err.Error())
		return err
	}
	return nil
}

func (m *Manager) sendCheck() {
	_ = m.send(protocol.TypeUpdateCheck, "", struct{}{})
}

func (m *Manager) result(r protocol.UpdateResult) error {
	if len(r.Reason) > protocol.MaxUpdateReasonLength {
		r.Reason = r.Reason[:protocol.MaxUpdateReasonLength]
	}
	m.cfg.Logger.Info("aggiornamento: esito", "versione", r.Version, "esito", r.Outcome, "motivo", r.Reason)
	return m.send(protocol.TypeUpdateResult, "", r)
}

// reportMailbox legge gli esiti lasciati dall'updater e dal ritorno e li riporta (D3 punti 3 e 4):
// ogni file si toglie SOLO dopo che il backend ha ricevuto il messaggio. Restituisce applied.json
// della versione in esecuzione, se c'è.
func (m *Manager) reportMailbox() *Pending {
	s := m.mb.Collect(m.cfg.Version)
	for _, w := range s.Warnings {
		m.cfg.Logger.Warn("aggiornamenti: cassetta delle lettere", "avviso", w)
	}
	for _, it := range s.Items {
		if err := m.result(it.Result); err != nil {
			m.cfg.Logger.Warn("aggiornamenti: esito non consegnato, resta nella cassetta", "versione", it.Result.Version, "esito", it.Result.Outcome, "errore", err.Error())
			continue
		}
		if err := it.Ack(); err != nil {
			m.cfg.Logger.Warn("aggiornamenti: esito consegnato ma non tolto dalla cassetta", "errore", err.Error())
		}
	}
	return s.Applied
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
	_ = m.send(protocol.TypeError, re, protocol.ErrorBody{Code: protocol.CodeMessageInvalid, Message: msg})
}

// offer tratta un update.available (D3 punto 1, D6): verifica, interruttori, download in sfondo.
func (m *Manager) offer(u protocol.UpdateAvailable) {
	version := u.Manifest.Version
	sig, err := protocol.DecodeSignature(u.Signature)
	if err != nil {
		_ = m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonBadSignature})
		return
	}
	if err := Verify(u.Manifest, sig, m.key, m.cfg.Platform); err != nil {
		m.cfg.Logger.Warn("aggiornamento rifiutato", "versione", version, "errore", err.Error())
		_ = m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonOf(err, ReasonBadSignature)})
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
		_ = m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateSkipped, Reason: skip})
		return
	}
	if busy {
		m.cfg.Logger.Info("aggiornamento già in corso: proposta ignorata", "versione", version)
		return
	}
	if m.mb.IsRegular(FileApplyRequest) {
		m.setBusy(false)
		m.cfg.Logger.Info("una richiesta di aggiornamento è già in attesa dell'updater: proposta ignorata", "versione", version)
		return
	}
	m.downloads.Add(1)
	go func() {
		defer m.downloads.Done()
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
	m.mb.RemoveDownload()
	if err := Download(context.Background(), m.cfg.HTTPClient, man.URL, man.SHA256, m.mb, m.cfg.DownloadTimeout); err != nil {
		m.cfg.Logger.Warn("aggiornamento: download rifiutato", "versione", version, "errore", err.Error())
		_ = m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonOf(err, ReasonDownloadFailed)})
		return
	}
	canon, err := Canonical(man)
	if err != nil {
		_ = m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonBadSignature})
		return
	}
	if err := m.mb.WriteRequest(canon, sig, version); err != nil {
		m.mb.RemoveDownload()
		m.cfg.Logger.Warn("aggiornamento: richiesta non scritta", "versione", version, "errore", err.Error())
		_ = m.result(protocol.UpdateResult{Version: version, Outcome: protocol.UpdateFailed, Reason: ReasonDownloadFailed})
		return
	}
	m.cfg.Logger.Info("aggiornamento preparato: l'updater lo verifica e lo installa", "versione", version, "cartella", m.cfg.Dir)
}
