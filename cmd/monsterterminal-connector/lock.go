package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockStateDir prende il lock esclusivo della cartella di stato per tutta la vita di run: due
// connettori sullo stesso stato si cancellerebbero a vicenda il socket degli hook e il giornale
// (TD-MT-CONNECTOR-SOCKET-LOCK, prova dal vivo del 2026-10-17). Il lock cade da solo alla fine del
// processo, anche brusca. Restituisce il file da tenere aperto.
func lockStateDir(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("un altro connettore è già in esecuzione su %s: fermarlo prima di avviarne un altro", stateDir)
		}
		return nil, err
	}
	return f, nil
}
