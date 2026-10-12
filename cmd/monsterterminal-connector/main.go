// Comando monsterterminal-connector: registra l'host e mantiene il collegamento in uscita.
//
//	monsterterminal-connector register   legge il token di installazione da MONSTERTERMINAL_INSTALL_TOKEN
//	                                     o dalla prima riga dello standard input
//	monsterterminal-connector run        si collega e resta collegato
//
// Il token di installazione non si passa mai come argomento: comparirebbe nell'elenco dei processi.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/hostinfo"
	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
	"github.com/ciarasino/monsterterminal-connector/internal/register"
	"github.com/ciarasino/monsterterminal-connector/internal/transport"
)

// version si imposta in compilazione con -ldflags "-X main.version=…".
var version = "0.1.0-dev"

const (
	defaultServer    = "https://connect.monsterterminal.com"
	defaultTokenFile = "/etc/monsterterminal/host-token"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	server := fs.String("server", defaultServer, "origine https del servizio")
	tokenFile := fs.String("token-file", defaultTokenFile, "file del token dell'host")
	_ = fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "register":
		err = cmdRegister(ctx, *server, *tokenFile)
	case "run":
		err = cmdRun(ctx, log, *server, *tokenFile)
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("terminato con errore", "errore", err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "uso: monsterterminal-connector register|run|version [-server URL] [-token-file PERCORSO]")
}

func readInstallToken() (string, error) {
	if t := os.Getenv("MONSTERTERMINAL_INSTALL_TOKEN"); t != "" {
		return t, nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("token di installazione mancante: impostare MONSTERTERMINAL_INSTALL_TOKEN o passarlo sullo standard input")
	}
	return strings.TrimSpace(line), nil
}

func cmdRegister(ctx context.Context, server, tokenFile string) error {
	if register.TokenExists(tokenFile) {
		return fmt.Errorf("l'host risulta già registrato (%s esiste): per registrarlo di nuovo rimuovere prima il file", tokenFile)
	}
	tok, err := readInstallToken()
	if err != nil {
		return err
	}
	hi, err := hostinfo.Collect()
	if err != nil {
		return err
	}
	c := register.Client{BaseURL: server, HTTPClient: &http.Client{Timeout: 30 * time.Second}}
	resp, err := c.Register(ctx, tok, register.Info{
		MachineID: hi.MachineID, Hostname: hi.Hostname, OS: "linux", Distro: hi.Distro,
		DistroVersion: hi.DistroVersion, Arch: hi.Arch, ConnectorVersion: version,
		RunAsUser: hi.User, RunAsRoot: hi.Root,
	})
	if err != nil {
		return err
	}
	if err := register.SaveToken(tokenFile, resp.HostToken); err != nil {
		return err
	}
	fmt.Printf("Host registrato per %s (id %s).\n", resp.TenantLabel, resp.HostID)
	return nil
}

func cmdRun(ctx context.Context, log *slog.Logger, server, tokenFile string) error {
	tok, err := register.LoadToken(tokenFile)
	if err != nil {
		return err
	}
	hi, err := hostinfo.Collect()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(server, "https://") {
		return errors.New("il servizio deve essere https")
	}
	return transport.Run(ctx, transport.Config{
		URL:        "wss://" + strings.TrimPrefix(server, "https://") + "/v1",
		Token:      tok,
		HTTPClient: &http.Client{},
		Logger:     log,
		Hello: func() protocol.Hello {
			return protocol.Hello{
				ConnectorVersion: version, Protocol: protocol.Version, OS: "linux", Arch: hi.Arch,
				RunAsUser: hi.User, RunAsRoot: hi.Root, Capabilities: []string{},
				Agents: []protocol.Agent{},
			}
		},
	})
}
