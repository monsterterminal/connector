# Changelog

## Schemi 0.2.0 — 2026-10-16

- Solo aggiunte rispetto a 0.1.0 (protocollo sempre 1): `waitingFor`/`requestId` in `agent.state`,
  `request.context`, `request.respond`, `request.respond.result`, `RATE_LIMITED` e `UNAVAILABLE` in
  `http/error.json`, tetto 2^53 − 1 sui contatori.

## Connettore — 2026-10-16 (non rilasciato)

- Stati degli agenti: adattatore di Claude Code con hook (`hook`, `hooks install`/`remove` col
  consenso), livello generico col timer di inattività, capacità `hooks` in `hello`.

## Connettore — 2026-10-15 (non rilasciato)

- Allineamento al gateway del backend (ADR 0409, verbale 2026-10-13): sostituzione con
  `X-MonsterTerminal-Host-Token` (`register -replace`), giornale degli eventi con `events.ack`,
  riallineamento al segnalibro del backend (`ACK_AHEAD`), `journal.truncated` nella forma coerente,
  invio dosato sotto i limiti per host, `429`/`503` con `Retry-After`, chiusura `1011`.
- Primo evento: `connector.restarted` a ogni avvio dopo il primo (update, rollback, host-reboot,
  service-restart, crash).
- `cmd/` testabile; codice di uscita `3` per host revocato.
- Terminale: canali sulle sessioni tmux (`session.open/resize/close`, `sessions.list`, frame binari,
  sola lettura, contropressione, `connector-shutdown`). Nuova dipendenza `github.com/creack/pty`.
- Versione di sviluppo `0.1.1-dev`; il giornale si azzera quando si registra un host nuovo.

## Schemi 0.1.0 — 2026-10-12

- Prima trascrizione degli schemi dal capitolo 1 della spec `monsterterminal-saas.md`, protocollo 1
  approvato dal BOSS il 2026-10-12.
