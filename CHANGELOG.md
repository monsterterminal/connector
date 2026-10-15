# Changelog

## Connettore — 2026-10-15 (non rilasciato)

- Allineamento al gateway del backend (ADR 0409, verbale 2026-10-13): sostituzione con
  `X-MonsterTerminal-Host-Token` (`register -replace`), giornale degli eventi con `events.ack`,
  riallineamento al segnalibro del backend (`ACK_AHEAD`), `journal.truncated` nella forma coerente,
  invio dosato sotto i limiti per host, `429`/`503` con `Retry-After`, chiusura `1011`.
- Primo evento: `connector.restarted` a ogni avvio dopo il primo (update, rollback, host-reboot,
  service-restart, crash).
- `cmd/` testabile; codice di uscita `3` per host revocato.

## Schemi 0.1.0 — 2026-10-12

- Prima trascrizione degli schemi dal capitolo 1 della spec `monsterterminal-saas.md`, protocollo 1
  approvato dal BOSS il 2026-10-12.
