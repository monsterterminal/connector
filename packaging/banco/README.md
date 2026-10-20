# Banco 2 — `install.sh` end-to-end in un container con systemd

Il banco esegue il comando di una riga del cliente su una «macchina» usa-e-getta (Ubuntu 24.04 con
systemd come PID 1) contro un rilascio **locale firmato** da una chiave Ed25519 generata al momento e
un **backend finto** per la registrazione. Non parla mai con la produzione: il download va solo a
`releases.banco` (nginx di banco), la registrazione solo a `connect.banco:8443` (`cmd/mt-fakeserver`),
entrambi sulla rete Docker `mt-banco`. Nessun segreto vero: chiavi, CA, certificati e token di
installazione nascono con `up` e muoiono con `down`.

Corrisponde alla proposta §3.2 della checklist del collaudo
(`docs/prove/collaudo-cliente-vero-monsterterminal-2026-10-19.md` nel repo monsterapp).

## Come si lancia

Serve una macchina con Docker, `openssl` ≥ 3 (per `-copy_extensions`), `sha256sum`, `python3`, `bash`.
Non serve Go: il connettore e il backend finto si compilano in `golang:1.27` (una build alla volta,
cache dei moduli e dei build in `~/.cache/go-banco`, riusata fra i giri).

```sh
bash packaging/banco/banco.sh up         # chiavi, CA, build, rilascio firmato, rete e tre container
bash packaging/banco/banco.sh negative   # firma sbagliata, token malformato, token sconosciuto
bash packaging/banco/banco.sh install    # il comando di una riga + asserzioni (B2, B3, B7, B8)
bash packaging/banco/banco.sh uninstall  # --uninstall --purge + asserzioni
bash packaging/banco/banco.sh status     # lo status-file del backend finto (JSON)
bash packaging/banco/banco.sh down       # rimuove tutto ciò che il banco ha creato (mt-banco-*)
bash packaging/banco/banco.sh all        # up, negative, install, uninstall in un colpo (poi: down)
```

L'ordine naturale è `up → negative → install → uninstall → down`: `negative` prova l'host pulito
(N3 vuole un host non registrato; se lo trova registrato la salta e lo dice). Ogni sottocomando
stampa le asserzioni (`ok`/`FAIL`), esce con 1 se una è rossa, e lascia le trascrizioni di
`install.sh` in `~/mt-banco/status/*.log`.

Variabili: `BANCO_HOME` (default `~/mt-banco`), `BANCO_VERSION` (`0.0.0-banco`), `BANCO_ASSETS`,
`BANCO_GO_IMAGE` (`golang:1.27`), `BANCO_GO_CACHE` (`~/.cache/go-banco`), `BANCO_CGROUPNS`
(`private`, oppure `host` che monta anche `/sys/fs/cgroup` della macchina).

### Due vie per il rilascio

1. **Build locale (default).** `up` compila `cmd/monsterterminal-connector` con gli stessi flag di
   `release.yml` (`CGO_ENABLED=0`, `-trimpath -ldflags "-s -w -buildid= -X main.version=$VERSION"`,
   più `-buildvcs=false` perché il sorgente copiato non ha `.git`) con `VERSION=0.0.0-banco`, e
   riempie `install.sh` con `sed s/@VERSION@/…/` come la CI. Due build consecutive hanno dato lo
   stesso SHA-256: la riproducibilità regge anche qui.
2. **Asset di una release già compilata** (per esempio la bozza `v0.2.0` prodotta dalla CI, scaricata
   con `gh release download v0.2.0 --repo monsterterminal/connector --dir …` da un `gh` autenticato):
   `BANCO_ASSETS=/cartella BANCO_VERSION=0.2.0 banco.sh up`. Il banco prende da lì il binario
   `monsterterminal-connector_0.2.0_linux_<arch>` e `install.sh`, compila solo il backend finto.

In entrambi i casi `up` **sostituisce** nello script la chiave pubblica `mt-updates-2026` e la sua
impronta con quelle della chiave del banco, in modo coerente, e ne fa la controprova (impronta
della chiave scritta = impronta dichiarata): così `check_tools` e `verify_signature` fanno il
lavoro vero, senza `--insecure-skip-signature`. `SHA256SUMS` è firmata con `openssl pkeyutl -sign
-rawin` (64 byte grezzi), la stessa forma di `scripts/firma-rilascio.sh`.

## Cosa prova (mappa alla checklist, fase B)

| Riga | Nel banco | Asserzioni |
|---|---|---|
| B2 | `curl -fsSL https://releases.banco/install.sh \| env MONSTERTERMINAL_INSTALL_TOKEN=… sh -s -- --server https://connect.banco:8443 --releases-url https://releases.banco/releases/download`, token letto dal token-file del backend finto e passato al container **solo come variabile d'ambiente** (`docker exec --env-file`) | firma e integrità verificate (righe dello script); utente `monsterterminal` di sistema con home; binario `0755 root` uguale all'asset (sha256) e `version` = versione del banco; `connector.conf` con `server=`, `installed_version=`, `user=`, `auto_update=yes`, `catalog_install=yes`; state-dir `0700`, `host-token` `0600` dell'utente e in forma `mth_`; unit `0644` con `User=monsterterminal`; «Host registrato per Acme» stampato; il token non compare nella trascrizione né in `/tmp` restano file |
| B3 | status-file del backend finto + journal del container | `is-active` = `active`, `is-enabled` = `enabled`; 1 registrazione con `hostname=cliente-banco`, `arch`, `connectorVersion`, `runAsUser=monsterterminal`, `runAsRoot=false`; ≥1 upgrade, ≥1 hello, ≥1 collegamento entro 30 s; journal con `msg=collegato hostId=… protocollo=1`; **nessun token** (`mti_`/`mth_`) nel journal, nell'argv del servizio (`ps -o args`) né nel suo `environ`; il processo gira come `monsterterminal` |
| B7 | secondo `install.sh` con lo stesso comando, senza `--replace` | «Host già registrato», ancora 1 registrazione, servizio attivo |
| B8 | `install.sh --replace` col secondo token | «Host sostituito», 2 registrazioni e la seconda con la prova di possesso (`X-MonsterTerminal-Host-Token`), `host-token` cambiato, il servizio riparte e si ricollega (≥2 collegamenti). Si osserva di passaggio anche il comportamento di B9: il processo vecchio riceve `4401`, esce con codice 3 e non ritenta |
| B10 | `negative` N3: token ben formato ma sconosciuto | `HTTP 401 INSTALL_TOKEN_INVALID`, «registrazione fallita», nessun `host-token`, nessuna unit, servizio non attivo, 0 registrazioni |
| §1.10 | `negative` N1: `SHA256SUMS.sig` firmata da **un'altra** chiave Ed25519 (firma perfetta, chiave sbagliata), su host pulito | «la firma del rilascio NON è valida», uscita ≠ 0, il binario **non viene scaricato** (contatore nginx), binario/conf/unit/token invariati, **l'utente `monsterterminal` non esiste** (la verifica precede `ensure_user`, PR #9); un utente lasciato da un giro precedente viene tolto prima con `userdel -r` |
| §1.14 | `negative` N2: token malformato (`mti_corto`) | «non ha la forma attesa», uscita ≠ 0, **nessuna richiesta** a `/releases/download/` |
| `--uninstall --purge` | `uninstall` | unit assente, servizio né attivo né abilitato, binario assente, `/etc/monsterterminal` e `/var/lib/monsterterminal` assenti, nessun processo del connettore; l'utente resta e lo script stampa il `userdel -r` da fare a mano |

`up` verifica anche il banco stesso: systemd `running` nel container, `machine-id` generato,
`releases.banco` raggiungibile in https con la CA del banco (`curl --proto '=https' --tlsv1.2`),
`connect.banco:8443` che risponde, token-file `0600` con tre token `mti_`, status-file iniziale vuoto.

L'header `X-MonsterTerminal-Schema` (ADR 0418 D5) dichiarato a ogni `register` e upgrade finisce
nello status-file (`declaredSchemas`, da `Server.DeclaredSchemas`) e viene stampato come informativo:
il banco non lo giudica.

## Cosa NON prova

- **La release firmata vera** (GitHub → `scripts/firma-rilascio.sh` → `install.sh` pubblico su
  `monsterterminal.com`): la chiave di produzione sta sul PC del BOSS. È la prova P8+P9, una volta
  per rilascio, dal cliente vero.
- **B0, B1** (flag `monsterterminal.hosts_ui`, dialogo «Genera token»), **B4** (pagina host), **B6**
  (terminale dal browser), **B9** dalla UI: vogliono il backend vero e la UI (banco 1 / collaudatore).
  Il backend finto risponde sempre «Acme» e non ha billing né kill-switch nel banco.
- **B5** (riavvio e riconnessione) non è un passo del banco; `--replace` lo attraversa di fatto.
- **tmux dal vivo** e il terminale sull'host (§1.7): il container ha `tmux` ma nessuna sessione si
  apre dal banco (serve il gateway vero).
- **B11, gli hook di Claude Code** (§1.21): `install.sh` non li installa, per scelta. Sono un passo a
  parte dell'utente, come utente del connettore:
  `sudo -u monsterterminal monsterterminal-connector hooks install -yes -state-dir /var/lib/monsterterminal`
  (e `hooks remove` per toglierli). Claude Code non gira nel banco.
- Le distribuzioni oltre Ubuntu 24.04 (`debian:12`, `rockylinux:9`): basta un secondo
  `Dockerfile.host`, ma non è fatto.

## Rilievi del primo giro (20/10/2026) e loro esito

Chiusi con la PR #9: `download_release` precede `ensure_user` (con una firma non valida non resta
nemmeno l'utente: asserzione N1), `--uninstall` dice che l'utente resta e stampa il `userdel -r`
(asserzione in `uninstall`). Resta, documentato: su `401 INSTALL_TOKEN_INVALID` (B10) il binario e
`connector.conf` restano installati (`install_files` precede `register_host`), nessuna unit e nessun
servizio — coerente con «token non consumato, riprova con un token nuovo».

## Come è fatto

- `Dockerfile.host`: `ubuntu:24.04` + `systemd systemd-sysv tmux curl openssl ca-certificates sudo
  procps`, `machine-id` vuoto (lo genera systemd), getty e logind mascherati, `CMD /lib/systemd/systemd`.
  Gira con `--privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock` (Docker 29, cgroup v2,
  systemd parte in ~1 s). **`--privileged` è ammesso solo perché il container è usa-e-getta, senza
  porte pubblicate, sulla rete del banco**; `BANCO_CGROUPNS=host` è il fallback con
  `-v /sys/fs/cgroup:/sys/fs/cgroup:rw` per Docker più vecchi.
- `Dockerfile.fakeserver`: `alpine:3.20` + il binario statico `mt-fakeserver` compilato da `up`.
  Gira con l'uid di chi lancia il banco e scrive token-file e status-file in `~/mt-banco/status/`.
- `nginx-releases.conf`: `nginx:1.27-alpine`, solo file statici in https:
  `/install.sh` e `/releases/download/v<versione>/{SHA256SUMS,SHA256SUMS.sig,<binario>,install.sh}`.
- CA e certificati: ECDSA P-256, 3 giorni, SAN `releases.banco` e `connect.banco`; la CA entra nel
  container host con `update-ca-certificates`, così sia `curl` sia il connettore (pool di sistema di
  Go) si fidano del banco e di nient'altro in più.
- `cmd/mt-fakeserver`: `internal/fakeserver` (lo stesso dei test) montato con `Serve` su un listener
  TLS proprio; flag `-listen`, `-tls-cert`, `-tls-key`, `-token-ttl`, `-tokens`, `-token-file`,
  `-status-file`. Lo status-file è riscritto (atomicamente) a ogni evento via `Server.Notify`.

## Costo

Sul VPS di sviluppo (8 vCPU, con i runner CI a carico 3-4): il primo `up` paga il pull di
`golang:1.27`, `nginx:1.27-alpine`, `ubuntu:24.04`, `alpine:3.20` e il download dei due moduli Go
(~2-3 minuti); a cache calda **l'intero giro `all` dura ~20 secondi** (build inclusa). `down` lascia
la cache Go (~340 MB) e le immagini di base; `down --images` toglie anche quelle.
