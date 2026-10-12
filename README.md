# MonsterTerminal Connector

Il connettore di MonsterTerminal: un piccolo programma installato sull'host del cliente, che apre una
connessione in uscita verso MonsterTerminal, registra l'host e lascia lavorare tmux e gli agenti AI
anche quando il collegamento cade.

> Stato: **prima fetta**. Registrazione dell'host e collegamento in uscita con heartbeat, ricollegamento,
> revoca e kill-switch. Non ci sono ancora terminale, eventi degli agenti, catalogo né aggiornamenti.

## Uso

```
# registrazione: il token di installazione si passa per variabile d'ambiente o standard input,
# mai come argomento (comparirebbe nell'elenco dei processi)
MONSTERTERMINAL_INSTALL_TOKEN=mti_… monsterterminal-connector register
# collegamento
monsterterminal-connector run
```

Opzioni comuni: `-server` (default `https://connect.monsterterminal.com`) e `-token-file` (default
`/etc/monsterterminal/host-token`, scritto con permessi `0600`).

## Riferimenti

- ADR 0409 — il connettore; ADR 0410 — agenti AI, stato comune, catalogo di ricette (monorepo MonsterApp).
- Spec `docs/spec/monsterterminal-saas.md`, capitolo 1: il protocollo tra connettore e backend.
- Schemi del protocollo: [`schemas/0.1.0/`](schemas/0.1.0/README.md).

## Requisiti

- Go 1.27 o superiore per la compilazione.
- Destinazione: Linux delle famiglie Debian/Ubuntu e Red Hat, x86-64 e ARM64. Eseguibile unico, senza
  dipendenze sull'host.

## Struttura prevista

| Percorso | Contenuto |
|---|---|
| `cmd/monsterterminal-connector/` | punto d'ingresso dell'eseguibile |
| `internal/protocol/` | busta, messaggi e frame binari, validati contro `schemas/` |
| `internal/transport/` | WebSocket in uscita, heartbeat, ricollegamento |
| `internal/register/` | registrazione con il token di installazione |
| `internal/tmux/` | canali del terminale sulle sessioni tmux |
| `internal/journal/` | giornale degli eventi sul disco dell'host |
| `internal/agents/` | adattatori degli agenti AI e timer di inattività |
| `internal/catalog/` | esecuzione delle ricette firmate |
| `internal/update/` | aggiornamento firmato e ritorno alla versione precedente |
| `internal/signing/` | verifica Ed25519 con le chiavi pubbliche incluse nel binario |
| `schemas/<versione>/` | JSON Schema del protocollo, fonte unica del contratto |
| `packaging/` | servizio systemd e script di installazione |

Le cartelle si creano quando arriva il codice che ci va dentro. Oggi esistono `cmd/monsterterminal-connector`,
`internal/protocol`, `internal/register`, `internal/transport`, `internal/hostinfo` e `internal/fakeserver`,
un backend finto che parla il protocollo 0.1.0 e serve solo ai test.

## Test

```
go test -race ./...
```

I test degli schemi controllano che ogni schema sia JSON valido, chiuso, con un `$id` coerente col
percorso e riferimenti risolti. I test del codice girano contro il backend finto su TLS: registrazione,
collegamento, caduta e ricollegamento, revoca, kill-switch, heartbeat, certificato non fidato.

## Licenza

Apache License 2.0: vedi [`LICENSE`](LICENSE) e [`NOTICE`](NOTICE).
