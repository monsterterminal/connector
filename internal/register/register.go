// Package register scambia il token di installazione con il token dell'host e lo conserva.
//
// Riferimento: spec monsterterminal-saas.md §1.3 (registrazione), §1.20 (formati), schemi
// schemas/0.1.0/http/register-*.json e http/error.json.
package register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

var (
	installTokenRE = regexp.MustCompile(`^mti_[A-Za-z0-9_-]{16,}$`)
	hostTokenRE    = regexp.MustCompile(`^mth_[A-Za-z0-9_-]{43,}$`)
)

// Info è il corpo di POST /v1/register. Il tenant non compare: lo decide il token (§1.3).
type Info struct {
	MachineID        string `json:"machineId"`
	Hostname         string `json:"hostname"`
	OS               string `json:"os"`
	Distro           string `json:"distro"`
	DistroVersion    string `json:"distroVersion"`
	Arch             string `json:"arch"`
	ConnectorVersion string `json:"connectorVersion"`
	RunAsUser        string `json:"runAsUser"`
	RunAsRoot        bool   `json:"runAsRoot"`
}

// Response è la risposta 201 di POST /v1/register.
type Response struct {
	HostID      string `json:"hostId"`
	HostToken   string `json:"hostToken"`
	TenantLabel string `json:"tenantLabel"`
	Protocol    int    `json:"protocol"`
}

// APIError è un rifiuto del backend con il suo codice (http/error.json).
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("registrazione rifiutata: HTTP %d %s", e.Status, e.Code)
	if h := e.Hint(); h != "" {
		msg += ": " + h
	}
	return msg
}

// Hint spiega all'amministratore cosa fare per ciascun codice (http/error.json, §1.3, §1.17).
func (e *APIError) Hint() string {
	switch e.Code {
	case "INSTALL_TOKEN_INVALID":
		return "token di installazione sconosciuto, scaduto o già usato; generarne uno nuovo dal pannello"
	case "BODY_INVALID":
		return "dati dell'host non accettati; il token di installazione NON è stato consumato e si può riusare dopo la correzione"
	case "HOST_DISABLED":
		return "questa macchina è fermata dal kill-switch del tenant; va riattivata dal pannello (il token di installazione NON è stato consumato)"
	case "SERVER_LIMIT":
		return "il tenant ha raggiunto gli host inclusi nel piano; attivare l'extra o revocare un host dal pannello"
	case "CONNECTOR_TOO_OLD":
		return "versione del connettore non più accettata; aggiornarlo e riprovare"
	case "RATE_LIMITED":
		return "troppi tentativi da questo indirizzo; riprovare fra qualche minuto"
	case "UNAVAILABLE":
		return "servizio momentaneamente non disponibile; riprovare"
	}
	return ""
}

// HeaderCurrentHostToken porta il token ATTUALE dell'host quando lo si sostituisce (§1.3,
// emendamento 2026-10-14): senza, o con un token che non è di questa macchina e di questo tenant,
// il backend crea un host nuovo accanto al vecchio.
const HeaderCurrentHostToken = "X-MonsterTerminal-Host-Token"

// ErrInstallTokenFormat segnala un token di installazione malformato, rifiutato prima della rete.
var ErrInstallTokenFormat = errors.New("token di installazione malformato: deve iniziare con mti_")

// Client chiama l'endpoint di registrazione.
type Client struct {
	// BaseURL è l'origine del servizio, ad esempio https://connect.monsterterminal.com.
	BaseURL    string
	HTTPClient *http.Client
}

// Register esegue lo scambio. Il token di installazione non compare mai negli errori restituiti.
func (c Client) Register(ctx context.Context, installToken string, info Info) (Response, error) {
	return c.Replace(ctx, installToken, "", info)
}

// Replace registra l'host sostituendo quello che possiede currentHostToken (stessa macchina, stesso
// tenant): il backend revoca il vecchio host e chiude la sua connessione. currentHostToken vuoto
// equivale a Register. Nessuno dei due token compare negli errori restituiti.
func (c Client) Replace(ctx context.Context, installToken, currentHostToken string, info Info) (Response, error) {
	var out Response
	if !installTokenRE.MatchString(installToken) {
		return out, ErrInstallTokenFormat
	}
	if currentHostToken != "" && !hostTokenRE.MatchString(currentHostToken) {
		return out, errors.New("token attuale dell'host malformato")
	}
	if !strings.HasPrefix(c.BaseURL, "https://") {
		return out, errors.New("il servizio deve essere https")
	}
	body, err := json.Marshal(info)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/register", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+installToken)
	if currentHostToken != "" {
		req.Header.Set(HeaderCurrentHostToken, currentHostToken)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("registrazione: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Code == "" {
			e.Code = "UNKNOWN"
		}
		return out, &APIError{Status: resp.StatusCode, Code: e.Code}
	}
	if err := protocol.DecodeStrict(raw, &out); err != nil {
		return out, fmt.Errorf("risposta di registrazione: %w", err)
	}
	if !protocol.IsUUID(out.HostID) || !hostTokenRE.MatchString(out.HostToken) || out.Protocol < 1 || len(out.TenantLabel) > 200 {
		return Response{}, fmt.Errorf("risposta di registrazione: %w", protocol.ErrInvalid)
	}
	return out, nil
}

// SaveToken scrive il token dell'host con permessi 0600, in modo atomico. La cartella, se manca,
// nasce con permessi 0700.
func SaveToken(path, token string) error {
	if !hostTokenRE.MatchString(token) {
		return errors.New("token dell'host malformato")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".host-token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
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
	return os.Rename(tmpName, path)
}

// ErrTokenFilePermissions segnala un file del token leggibile da altri utenti.
var ErrTokenFilePermissions = errors.New("il file del token dell'host è accessibile ad altri utenti: permessi attesi 0600")

// LoadToken legge il token dell'host e rifiuta un file con permessi larghi.
func LoadToken(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", ErrTokenFilePermissions
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if !hostTokenRE.MatchString(tok) {
		return "", errors.New("token dell'host malformato")
	}
	return tok, nil
}

// TokenExists dice se il file del token esiste già.
func TokenExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
