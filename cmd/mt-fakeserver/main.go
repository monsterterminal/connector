// Comando mt-fakeserver: il backend finto di internal/fakeserver servito su un indirizzo e un
// certificato propri, per il banco di prova di install.sh con systemd (packaging/banco). Solo per le
// prove: non parla con nessun backend vero e non va mai esposto.
//
//	mt-fakeserver -listen :8443 -tls-cert connect.crt -tls-key connect.key \
//	    -token-file /banco/install-token -status-file /banco/status.json
//
// All'avvio conia i token di installazione (uno per riga nel token-file, 0600; senza -token-file li
// stampa su stdout come TOKEN=mti_…) e riscrive lo status-file in JSON a ogni evento osservato
// (registrazioni con hostname, arch e versione; upgrade; collegamenti; hello; update.check e
// update.result; i connector.restarted), così il banco asserisce leggendo un file invece di
// interpretare i log. I log umani vanno su stderr.
//
// Con -command-dir CARTELLA il banco comanda il backend finto scrivendo file JSON nella cartella
// (consumati e cancellati, in ordine di nome, ogni 250 ms), senza porte né rete in più:
//
//	{"action":"push","manifest":{…},"signature":"base64"}   update.available a tutti gli host collegati
//	{"action":"offer","manifest":{…},"signature":"base64"}  risposta ai prossimi update.check (senza
//	                                                         manifest: torna a update.none)
//	{"action":"auto-update","value":false}                   interruttore del backend (ADR 0438 D6),
//	                                                         anche in config.update subito; senza value
//	                                                         il campo torna assente (acceso)
//	{"action":"session.open","session":"nome"}               session.open (crea) su tutti gli host
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/register"
)

// registration è una riga dello status-file: la registrazione come l'ha vista il server, più se il
// connettore ha chiesto la sostituzione di un host.
type registration struct {
	Hostname         string `json:"hostname"`
	Distro           string `json:"distro"`
	DistroVersion    string `json:"distroVersion"`
	Arch             string `json:"arch"`
	ConnectorVersion string `json:"connectorVersion"`
	RunAsUser        string `json:"runAsUser"`
	RunAsRoot        bool   `json:"runAsRoot"`
	Replace          bool   `json:"replace"`
}

// status è lo status-file: cresce solo, viene riscritto per intero a ogni evento.
type status struct {
	UpdatedAt     string         `json:"updatedAt"`
	TokensMinted  int            `json:"tokensMinted"`
	Registrations []registration `json:"registrations"`
	// DeclaredSchemas è l'header X-MonsterTerminal-Schema di ogni register e upgrade, in ordine
	// (ADR 0418 D5): informativo, il banco non lo giudica.
	DeclaredSchemas []string        `json:"declaredSchemas"`
	Upgrades        int             `json:"upgrades"`
	Connects        int             `json:"connects"`
	Hellos          int             `json:"hellos"`
	LastHello       *protocol.Hello `json:"lastHello,omitempty"`
	// Aggiornamento automatico (ADR 0438): update.check ricevuti, update.none mandati, update.result
	// ricevuti e i connector.restarted (motivo update/rollback) degli host.
	UpdateChecks  int                           `json:"updateChecks"`
	UpdateNones   int                           `json:"updateNones"`
	UpdateResults []protocol.UpdateResult       `json:"updateResults"`
	Restarts      []protocol.ConnectorRestarted `json:"restarts"`
	CommandsRun   int                           `json:"commandsRun"`
}

// command è un file di comando del banco (-command-dir).
type command struct {
	Action    string                   `json:"action"`
	Manifest  *protocol.UpdateManifest `json:"manifest,omitempty"`
	Signature string                   `json:"signature,omitempty"`
	Value     *bool                    `json:"value,omitempty"`
	Session   string                   `json:"session,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("mt-fakeserver", flag.ContinueOnError)
	listen := fs.String("listen", ":8443", "indirizzo di ascolto https")
	certFile := fs.String("tls-cert", "", "certificato TLS in PEM (senza: autofirmato in memoria)")
	keyFile := fs.String("tls-key", "", "chiave del certificato in PEM")
	tokenTTL := fs.Duration("token-ttl", time.Hour, "validità dei token di installazione coniati")
	tokens := fs.Int("tokens", 1, "quanti token di installazione coniare all'avvio")
	tokenFile := fs.String("token-file", "", "file 0600 con i token coniati, uno per riga (senza: stdout)")
	statusFile := fs.String("status-file", "", "file JSON riscritto a ogni evento")
	otherTokens := fs.Int("other-tokens", 0, "token di installazione di un ALTRO spazio di lavoro (ADR 0409 A2)")
	otherTokenFile := fs.String("other-token-file", "", "file 0600 dei token dell'altro spazio (obbligatorio con -other-tokens)")
	commandDir := fs.String("command-dir", "", "cartella dei file di comando del banco (aggiornamento automatico)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*certFile == "") != (*keyFile == "") {
		fmt.Fprintln(os.Stderr, "mt-fakeserver: -tls-cert e -tls-key vanno insieme")
		return 2
	}
	if *otherTokens < 0 || (*otherTokens > 0) != (*otherTokenFile != "") {
		fmt.Fprintln(os.Stderr, "mt-fakeserver: -other-tokens e -other-token-file vanno insieme")
		return 2
	}
	if *tokens < 1 {
		fmt.Fprintln(os.Stderr, "mt-fakeserver: -tokens deve essere almeno 1")
		return 2
	}

	cert, err := loadOrMakeCert(*certFile, *keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
		return 1
	}

	s := fakeserver.NewDetached()
	s.PingInterval = 20 * time.Second
	w := &statusWriter{path: *statusFile, s: s}
	w.st.TokensMinted = *tokens
	s.Notify = w.onEvent

	minted := make([]string, 0, *tokens)
	for i := 0; i < *tokens; i++ {
		tok, err := mintInstallToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
			return 1
		}
		s.AddInstallToken(tok, *tokenTTL)
		minted = append(minted, tok)
	}
	if err := publishTokens(*tokenFile, minted); err != nil {
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
		return 1
	}
	if *otherTokens > 0 {
		other := make([]string, 0, *otherTokens)
		for i := 0; i < *otherTokens; i++ {
			tok, err := mintInstallToken()
			if err != nil {
				fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
				return 1
			}
			s.AddInstallTokenFor(tok, *tokenTTL, "altro-spazio")
			other = append(other, tok)
		}
		if err := publishTokens(*otherTokenFile, other); err != nil {
			fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
			return 1
		}
	}
	if err := w.write(); err != nil {
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
		return 1
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
		return 1
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	fmt.Fprintf(os.Stderr, "mt-fakeserver: in ascolto su https://%s (token coniati: %d, validi %s)\n", ln.Addr(), *tokens, *tokenTTL)

	// Il canale degli hello va svuotato, altrimenti dopo 64 hello il server si blocca.
	go func() {
		for h := range s.Hellos() {
			w.onHello(h)
		}
	}()
	if *commandDir != "" {
		if err := os.MkdirAll(*commandDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
			return 1
		}
		go pollCommands(*commandDir, w, 250*time.Millisecond)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- s.Serve(tlsLn) }()
	select {
	case <-stop:
		fmt.Fprintln(os.Stderr, "mt-fakeserver: fermo")
		tlsLn.Close()
		s.Close()
		return 0
	case err := <-done:
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
		return 1
	}
}

// mintInstallToken conia un token nella forma del backend (connector-tokens.ts): mti_ + base64url di
// 32 byte casuali, 43 caratteri dopo il prefisso.
func mintInstallToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("token di installazione: %w", err)
	}
	return "mti_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// publishTokens scrive i token nel file (0600, creato in una cartella esistente) o li stampa.
func publishTokens(path string, toks []string) error {
	if path == "" {
		for _, t := range toks {
			fmt.Fprintf(os.Stdout, "TOKEN=%s\n", t)
		}
		return nil
	}
	var body []byte
	for _, t := range toks {
		body = append(body, t...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("token-file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "mt-fakeserver: %d token in %s\n", len(toks), path)
	return nil
}

type statusWriter struct {
	path string
	s    *fakeserver.Server
	mu   sync.Mutex
	st   status
}

func (w *statusWriter) onEvent(event string) {
	fmt.Fprintf(os.Stderr, "mt-fakeserver: evento %s\n", event)
	if event == "hello" {
		return // contato da onHello, quando il corpo è disponibile
	}
	if err := w.write(); err != nil {
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
	}
}

func (w *statusWriter) onHello(h protocol.Hello) {
	w.mu.Lock()
	w.st.Hellos++
	w.st.LastHello = &h
	w.mu.Unlock()
	fmt.Fprintf(os.Stderr, "mt-fakeserver: hello versione=%s arch=%s utente=%s capacità=%v\n", h.ConnectorVersion, h.Arch, h.RunAsUser, h.Capabilities)
	if err := w.write(); err != nil {
		fmt.Fprintln(os.Stderr, "mt-fakeserver:", err)
	}
}

// pollCommands legge e consuma i file di comando, in ordine di nome, a ogni intervallo.
func pollCommands(dir string, w *statusWriter, every time.Duration) {
	for {
		time.Sleep(every)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			_ = os.Remove(path)
			if err != nil {
				continue
			}
			if err := w.runCommand(raw); err != nil {
				fmt.Fprintf(os.Stderr, "mt-fakeserver: comando %s: %v\n", e.Name(), err)
			} else {
				fmt.Fprintf(os.Stderr, "mt-fakeserver: comando %s eseguito\n", e.Name())
			}
		}
	}
}

// runCommand esegue un comando del banco e riscrive lo status-file.
func (w *statusWriter) runCommand(raw []byte) error {
	var c command
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	switch c.Action {
	case "push":
		if c.Manifest == nil {
			return errors.New("push senza manifest")
		}
		n := w.s.PushUpdate(fakeserver.Offer{Manifest: *c.Manifest, Signature: c.Signature})
		fmt.Fprintf(os.Stderr, "mt-fakeserver: update.available %s spinto a %d host\n", c.Manifest.Version, n)
	case "offer":
		if c.Manifest == nil {
			w.s.SetOffer(nil)
		} else {
			w.s.SetOffer(&fakeserver.Offer{Manifest: *c.Manifest, Signature: c.Signature})
		}
	case "auto-update":
		w.s.SetAutoUpdate(c.Value)
	case "session.open":
		if c.Session == "" {
			return errors.New("session.open senza session")
		}
		n := w.s.OpenSessionAll(c.Session)
		fmt.Fprintf(os.Stderr, "mt-fakeserver: session.open %s a %d host\n", c.Session, n)
	default:
		return fmt.Errorf("azione sconosciuta %q", c.Action)
	}
	w.mu.Lock()
	w.st.CommandsRun++
	w.mu.Unlock()
	return w.write()
}

// write riscrive lo status-file per intero, in modo atomico (file accanto + rename).
func (w *statusWriter) write() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	regs := w.s.Registrations()
	replaces := w.s.ReplaceHeaders()
	w.st.Registrations = make([]registration, 0, len(regs))
	for i, r := range regs {
		replace := ""
		if i < len(replaces) {
			replace = replaces[i]
		}
		w.st.Registrations = append(w.st.Registrations, toRegistration(r, replace))
	}
	w.st.DeclaredSchemas = w.s.DeclaredSchemas()
	if w.st.DeclaredSchemas == nil {
		w.st.DeclaredSchemas = []string{}
	}
	w.st.Upgrades = w.s.Upgrades()
	w.st.Connects = w.s.Connects()
	w.st.UpdateChecks = w.s.UpdateChecks()
	w.st.UpdateNones = w.s.UpdateNones()
	if w.st.UpdateResults = w.s.AllUpdateResults(); w.st.UpdateResults == nil {
		w.st.UpdateResults = []protocol.UpdateResult{}
	}
	if w.st.Restarts = w.s.Restarts(); w.st.Restarts == nil {
		w.st.Restarts = []protocol.ConnectorRestarted{}
	}
	w.st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if w.path == "" {
		return nil
	}
	body, err := json.MarshalIndent(w.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("status-file: %w", err)
	}
	return os.Rename(tmp, w.path)
}

// toRegistration proietta il corpo di registrazione (register.Info, il contratto col backend)
// nella riga dello status-file; replace è l'header del token attuale, vuoto se non c'era.
func toRegistration(r register.Info, replace string) registration {
	return registration{Hostname: r.Hostname, Distro: r.Distro, DistroVersion: r.DistroVersion, Arch: r.Arch,
		ConnectorVersion: r.ConnectorVersion, RunAsUser: r.RunAsUser, RunAsRoot: r.RunAsRoot, Replace: replace != ""}
}

// loadOrMakeCert carica la coppia PEM o, senza file, genera un certificato autofirmato in memoria
// per 127.0.0.1 e localhost (comodo da solo, inutile dietro una CA: il banco passa i suoi file).
func loadOrMakeCert(certFile, keyFile string) (tls.Certificate, error) {
	if certFile != "" {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("certificato %s: %w", filepath.Base(certFile), err)
		}
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mt-fakeserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
