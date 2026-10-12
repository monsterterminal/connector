# MonsterTerminal Connector

Il connettore di MonsterTerminal: un piccolo programma installato sull'host del cliente, che apre una
connessione in uscita verso MonsterTerminal, registra l'host e lascia lavorare tmux e gli agenti AI
anche quando il collegamento cade.

> Stato: **preparazione**. Il codice del connettore non è ancora scritto. Il repository contiene gli
> schemi del protocollo e la struttura di partenza.

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

Le cartelle si creano quando arriva il codice che ci va dentro.

## Verifica degli schemi

```
go test ./...
```

Il test controlla che ogni schema sia JSON valido, chiuso, con un `$id` coerente col percorso e con
riferimenti che puntano a definizioni esistenti.

## Licenza

Apache License 2.0: vedi [`LICENSE`](LICENSE) e [`NOTICE`](NOTICE).
