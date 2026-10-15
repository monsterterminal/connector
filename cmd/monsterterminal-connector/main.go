// Comando monsterterminal-connector: registra l'host e mantiene il collegamento in uscita.
//
//	monsterterminal-connector register [-replace]   legge il token di installazione da
//	                                                MONSTERTERMINAL_INSTALL_TOKEN o dalla prima
//	                                                riga dello standard input
//	monsterterminal-connector run                   si collega e resta collegato
//	monsterterminal-connector version
//
// Il token di installazione non si passa mai come argomento: comparirebbe nell'elenco dei processi.
//
// Codici di uscita: 0 fine regolare, 1 errore, 2 uso sbagliato, 3 host revocato o token dell'host
// non valido (ricollegarsi non serve: occorre una nuova registrazione; il servizio non va riavviato).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// version si imposta in compilazione con -ldflags "-X main.version=…".
var version = "0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := defaultApp().run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
