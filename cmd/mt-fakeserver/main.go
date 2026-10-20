// Comando mt-fakeserver: il backend finto di internal/fakeserver servito su un indirizzo e un
// certificato propri, per il banco di prova di install.sh con systemd (packaging/banco). Solo per le
// prove: non parla con nessun backend vero e non va mai esposto.
//
//	mt-fakeserver -listen :8443 -tls-cert connect.crt -tls-key connect.key \
//	    -token-file /banco/install-token -status-file /banco/status.json
//
// All'avvio conia i token di installazione (uno per riga nel token-file, 0600; senza -token-file li
// stampa su stdout come TOKEN=mti_…) e riscrive lo status-file in JSON a ogni evento osservato
// (registrazioni con hostname, arch e versione; upgrade; collegamenti; hello), così il banco
// asserisce leggendo un file invece di interpretare i log. I log umani vanno su stderr.
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
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/register"
)

// registration è una riga dello status-file: la registrazione come l'ha vista il server, più
// l'header dello schema e se il connettore ha chiesto la sostituzione di un host.
type registration struct {
	Hostname         string `json:"hostname"`
	Distro           string `json:"distro"`
	DistroVersion    string `json:"distroVersion"`
	Arch             string `json:"arch"`
	ConnectorVersion string `json:"connectorVersion"`
	RunAsUser        string `json:"runAsUser"`
	RunAsRoot        bool   `json:"runAsRoot"`
	SchemaHeader     string `json:"schemaHeader"`
	Replace          bool   `json:"replace"`
}

// status è lo status-file: cresce solo, viene riscritto per intero a ogni evento.
type status struct {
	UpdatedAt     string          `json:"updatedAt"`
	TokensMinted  int             `json:"tokensMinted"`
	Registrations []registration  `json:"registrations"`
	Upgrades      int             `json:"upgrades"`
	Connects      int             `json:"connects"`
	Hellos        int             `json:"hellos"`
	LastHello     *protocol.Hello `json:"lastHello,omitempty"`
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*certFile == "") != (*keyFile == "") {
		fmt.Fprintln(os.Stderr, "mt-fakeserver: -tls-cert e -tls-key vanno insieme")
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

// write riscrive lo status-file per intero, in modo atomico (file accanto + rename).
func (w *statusWriter) write() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	regs := w.s.Registrations()
	replaces := w.s.ReplaceHeaders()
	schemas := w.s.SchemaHeaders()
	w.st.Registrations = make([]registration, 0, len(regs))
	for i, r := range regs {
		schema, replace := "", ""
		if i < len(schemas) {
			schema = schemas[i]
		}
		if i < len(replaces) {
			replace = replaces[i]
		}
		w.st.Registrations = append(w.st.Registrations, toRegistration(r, schema, replace))
	}
	w.st.Upgrades = w.s.Upgrades()
	w.st.Connects = w.s.Connects()
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
func toRegistration(r register.Info, schema, replace string) registration {
	return registration{Hostname: r.Hostname, Distro: r.Distro, DistroVersion: r.DistroVersion, Arch: r.Arch,
		ConnectorVersion: r.ConnectorVersion, RunAsUser: r.RunAsUser, RunAsRoot: r.RunAsRoot,
		SchemaHeader: schema, Replace: replace != ""}
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
