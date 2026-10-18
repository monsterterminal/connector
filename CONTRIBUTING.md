# Contribuire

Grazie dell'interesse. Il connettore è software libero sotto licenza Apache 2.0: ogni contributo che
invii è concesso con la stessa licenza (sezione 5 della licenza).

## Prima di aprire una PR

- Apri prima una issue per i cambiamenti che toccano il protocollo (`schemas/`) o il comportamento
  verso il backend: il contratto è versionato e il backend di MonsterTerminal lo segue, quindi una
  modifica va concordata.
- Correzioni di bug, test e documentazione si possono mandare direttamente.
- Per le vulnerabilità vedi [`SECURITY.md`](SECURITY.md): non aprire una issue pubblica.

## Sviluppo

```
go test -race ./...
go vet ./...
```

Serve Go 1.27 o superiore e, per i test del terminale, `tmux` (senza, quei test si saltano). I test
girano contro un backend finto locale (`internal/fakeserver`), non contro `monsterterminal.com`.

## Convenzioni

- La lingua del progetto, codice a parte, è l'italiano: commenti, documentazione, messaggi di commit.
  Le PR in inglese sono benvenute lo stesso.
- Messaggi di commit nella forma `tipo(ambito): cosa cambia` (`feat`, `fix`, `docs`, `test`,
  `chore`), come nella storia del repository.
- Ogni cambiamento di comportamento porta il suo test. Gli schemi hanno i loro test in
  `schemas/schemas_test.go`: una nuova versione degli schemi è una nuova cartella, mai una modifica a
  una versione già pubblicata.
- Niente token, host o dati reali nei test e nei file di esempio.
