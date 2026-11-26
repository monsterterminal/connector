# Ricette del catalogo

Le ricette con cui MonsterTerminal installa un agente su un server del cliente (spec `monsterterminal-saas.md`
§1.9 e §2.16 del monorepo MonsterApp, ADR 0410 D4 e A4, schemi `schemas/0.11.0/objects/recipe.json`).
Vivono qui, nel repository pubblico del connettore, una per cartella:

```
recipes/<recipeId>/<recipeVersion>.json       la ricetta, nei byte canonici che si firmano
recipes/<recipeId>/<recipeVersion>.json.sig   la firma Ed25519 del BOSS, in base64 su una riga
```

Il connettore le esegue con l'utente del connettore, senza shell, solo dopo aver verificato la firma con
la chiave pubblica `mt-recipes-2026` incorporata nel binario (`internal/signing`). Una ricetta cambiata
di un byte dopo la firma è `signature-invalid` sull'host: per questo il file è la **forma canonica**
(chiavi ordinate, senza spazi, senza a capo finale, cioè `jq -cjS .`) e si firma **il file**, non il suo
significato.

## Come nasce una ricetta

1. **PR** su questo repository con `recipes/<recipeId>/<recipeVersion>.json` (senza `.sig`). Chi la
   scrive verifica il canale ufficiale del vendor **in quel momento** e lo scrive in `source`; nella
   PR dichiara canale, versione, URL, checksum e da dove viene il checksum (sezione «Da verificare dal
   BOSS prima della firma»). La CI (`ricette` in `.github/workflows/ci.yml`) valida ogni ricetta con
   `scripts/valida-ricetta.sh`: schema 0.11.0, forma canonica, percorso coerente, scadenza entro 180
   giorni, niente `latest` né `http://`, `source` coerente coi passi, `hooks install -yes` ultimo passo
   dove `adapter.kind` è `hooks`.
2. **Revisione** e merge della bozza (il merge **non** la rende installabile: senza `.sig` il backend
   non la importa).
3. **Firma del BOSS, sul suo PC** (Jack, WSL), mai sul server né in CI:
   ```sh
   scripts/firma-ricetta.sh <recipeId> <recipeVersion>
   ```
   Lo script rivalida la ricetta, chiede la passphrase di `~/chiavi-monsterterminal/mt-recipes-2026.key`
   una volta a video, controlla che la chiave sia proprio `mt-recipes-2026` (impronta), firma i byte
   esatti del file, verifica subito la firma e scrive `<recipeVersion>.json.sig`. Poi una seconda PR
   con il solo `.sig` (la CI lo verifica con `--verifica`).
4. **Import dal super** nel pannello di MonsterApp: il backend scarica la coppia dal repository
   (URL fissato, come l'import dei rilasci), verifica la firma con la stessa chiave pubblica e salva i
   byte tal quali. Il super può solo abilitare o disabilitare ciò che ha importato: non modifica mai
   una ricetta.

Una ricetta cambiata è una **`recipeVersion` nuova** (`2.json`, `3.json`, …): la precedente resta nel
repository come storia, e sull'host il connettore rifiuta con `downgrade` una versione non maggiore
dell'ultima installata. Non si riscrive mai un file già firmato.

## Cosa verifica il BOSS prima di firmare

La firma dice «questa ricetta installa davvero quell'agente, in quella versione, da quel canale». Prima
di scrivere la passphrase il BOSS controlla, con le sue mani e non su parola di chi ha aperto la PR:

- **la versione** è quella pubblicata dal vendor che si vuole distribuire (e non `latest`);
- **il checksum** di ogni passo `fetch` è quello **pubblicato dal vendor** per quell'artefatto
  (`SHA256SUMS` o `codex-package_SHA256SUMS` nella release, `manifest.json` firmato GPG per Claude
  Code), oppure, se il vendor non ne pubblica, lo ricalcola scaricando l'artefatto dall'URL versionato
  della ricetta: `curl -fsSL <url> | sha256sum`;
- **per il canale npm** la versione fissata nel passo `npm install -g <pacchetto>@<versione>` è quella
  di `source.ref` ed esiste nel registro: `npm view <pacchetto>@<versione> version dist.integrity`;
- **`source`** dice la verità sul canale, e il canale è quello **ufficiale** del vendor (sotto);
- **`expiresAt`** è entro 180 giorni dal giorno della firma (lo script lo rifiuta altrimenti).

## La regola del canale ufficiale (decisione BOSS 2026-11-26)

Per ogni agente si usa il canale d'installazione **ufficiale** del vendor, con versione fissata e
verifica di checksum o firma:

- **npm dove npm è il canale ufficiale**: `npm install -g <pacchetto>@<versione> --prefix .npm-global`
  (percorso relativo alla home dell'utente del connettore) seguito da `catalog path-add .npm-global/bin`;
  `requires` porta `node` (≥ 22 dove il pacchetto lo chiede) e `npm`. Node è richiesto **solo** da
  queste ricette.
- **Altrimenti l'installer ufficiale, ma mai uno script non versionato**: non si scarica né si esegue
  `install.sh`, si scarica con `fetch` l'**artefatto versionato** che quell'installer scaricherebbe (il
  binario o l'archivio della release, con la versione nell'URL) e il suo `sha256` entra nella ricetta e
  quindi nella firma; poi `catalog extract` per gli archivi e `catalog path-add` per il `PATH`. Mai
  `curl | bash`, mai `tar` via shell.
- Se l'artefatto cambia per architettura, la ricetta è **una per architettura** (`<agente>-amd64`,
  `<agente>-arm64`, `platforms` disgiunti): lo schema ha un solo elenco di passi per ricetta e un passo
  `fetch` ha un solo URL. Il backend sceglie la ricetta che corrisponde alla piattaforma dell'host.

## I 180 giorni

`expiresAt` è obbligatorio e sta dentro la firma: oltre quell'istante il connettore rifiuta con
`expired`. Il tetto è 180 giorni dalla firma (spec §1.9, ADR 0410 A4 decisione 3): una ricetta non è
un'autorizzazione perpetua a installare una versione che fra sei mesi avrà vulnerabilità note. Chi
scrive la bozza mette oggi + 180 giorni (`date -u -d '+180 days' +%Y-%m-%dT00:00:00Z`); il giorno della
firma lo script verifica che la scadenza stia ancora nel tetto. Rinnovare una ricetta è scriverne la
versione successiva con la versione corrente dell'agente, non spostare la data.

## Le ricette di oggi

| Ricetta | Agente | Canale | Versione | Node |
|---|---|---|---|---|
| `claude-code/1` | Claude Code | npm `@anthropic-ai/claude-code@2.1.332` (vedi nota) | 2.1.332 | sì, ≥ 22 |
| `codex-amd64/1`, `codex-arm64/1` | Codex CLI | release GitHub `openai/codex` `rust-v0.172.0`, `codex-package-<target>.tar.gz` (checksum in `codex-package_SHA256SUMS`) | 0.172.0 | no |
| `opencode-amd64/1`, `opencode-arm64/1` | OpenCode | release GitHub `anomalyco/opencode` `v2.4.2`, `opencode-linux-<arch>.tar.gz` (checksum in `SHA256SUMS`) | 2.4.2 | no |

Nota su Claude Code: il vendor indica come «Recommended» l'installer nativo, che scarica il binario
versionato da `downloads.claude.ai/claude-code-releases/<versione>/<piattaforma>/claude` con il
checksum nel `manifest.json` firmato GPG. Quel binario pesa 367-384 MiB per Linux, oltre il tetto di
256 MiB di un passo `fetch` (§1.9): finché il tetto non cambia, la ricetta usa il pacchetto npm, che il
vendor documenta e che installa **lo stesso binario nativo** attraverso la dipendenza per piattaforma.

## Strumenti

- `scripts/valida-ricetta.sh FILE...` — validazione (usa `cmd/mt-valida-ricetta`, che riusa il tipo e la
  `Validate` del connettore: ciò che passa qui è ciò che l'host accetta). `--adesso ISTANTE` fissa
  l'orologio, solo per le prove.
- `scripts/firma-ricetta.sh <recipeId> <recipeVersion> [--key PERCORSO] [--sovrascrivi]` — firma;
  `--verifica` controlla un `.sig` esistente con la chiave pubblica incorporata, senza chiave privata.
- `internal/catalog/recipes_repo_test.go` — test di corrispondenza sulle ricette vere: schema, forma
  canonica, `source` coerente, `adapter.kind` per agente, firma dove c'è.
- Banchi: `sh scripts/valida-ricetta_test.sh`, `sh scripts/firma-ricetta_test.sh` (chiave usa e getta).
