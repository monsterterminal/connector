#!/bin/sh
# Installazione del connettore di MonsterTerminal (spec monsterterminal-saas.md §1.14, ADR 0409 D5-D7).
#
#   curl -fsSL https://monsterterminal.com/install.sh | sudo env MONSTERTERMINAL_INSTALL_TOKEN=mti_… sh
#   curl -fsSL https://monsterterminal.com/install.sh | sudo env MONSTERTERMINAL_INSTALL_TOKEN=mti_… sh -s -- --user mario
#
# Il token di installazione arriva SOLO dalla variabile d'ambiente: mai come argomento (comparirebbe
# nell'elenco dei processi), mai stampato, mai scritto nell'ambiente del servizio. Tutto il lavoro
# sta dentro funzioni e `main` è l'ultima riga: uno script scaricato a metà non esegue nulla.
#
# POSIX sh (gira su dash e sulla sh di RHEL). Niente bashismi: lo verifica checkbashisms in CI.
set -eu

# Riempiti dal workflow di rilascio (release.yml) e firmati dal BOSS insieme agli altri asset.
MT_VERSION="@VERSION@"
MT_DEFAULT_RELEASES="https://github.com/monsterterminal/connector/releases/download"
# Chiave pubblica Ed25519 degli aggiornamenti «mt-updates-2026», PEM (§1.10, §1.17): la stessa di
# SECURITY.md e di internal/signing nel binario. La chiave privata vive fuori dal server, sul PC di
# chi custodisce i rilasci. Se fosse vuota lo script rifiuterebbe di installare, salvo
# --insecure-skip-signature, che è una scelta esplicita per le istanze di prova.
MT_UPDATES_PUBKEY_PEM="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAkb+An530Ndo71tCM0i6DByEV+XWkb4xo4/EC3wDWKlk=
-----END PUBLIC KEY-----"
# Impronta SHA-256 del DER della chiave, per il confronto a occhio con SECURITY.md.
MT_UPDATES_PUBKEY_FINGERPRINT=90efb21f2321b1a530efff3973b5d8f4f354675af509958f3f08422cd1c43b0d

MT_BIN=/usr/local/bin/monsterterminal-connector
MT_ETC=/etc/monsterterminal
MT_CONF=$MT_ETC/connector.conf
MT_STATE=/var/lib/monsterterminal
MT_TOKEN_FILE=$MT_STATE/host-token
MT_UNIT_NAME=monsterterminal-connector
MT_UNIT=/etc/systemd/system/$MT_UNIT_NAME.service
MT_DEFAULT_USER=monsterterminal
MT_DEFAULT_SERVER=https://connect.monsterterminal.com

# Stato della corsa (riempito da parse_args).
MT_USER=$MT_DEFAULT_USER
MT_USER_SET=no
MT_ACCEPT_ROOT=no
MT_AUTO_UPDATE=yes
MT_CATALOG=yes
MT_SERVER=$MT_DEFAULT_SERVER
MT_RELEASES=$MT_DEFAULT_RELEASES
MT_REPLACE=no
MT_UNINSTALL=no
MT_PURGE=no
MT_DRY_RUN=no
MT_SKIP_SIGNATURE=no
MT_WANT_VERSION=""

msg() { printf '%s\n' "$*" >&2; }
die() { msg "install.sh: $*"; exit 1; }
usage_die() { msg "install.sh: $*"; usage; exit 2; }

usage() {
	cat >&2 <<'USAGE'
uso: sh install.sh [opzioni]        (il token di installazione in MONSTERTERMINAL_INSTALL_TOKEN)
  --user NOME             utente del connettore: esistente, oppure «monsterterminal» (default,
                          creato dallo script). Il connettore vede le sessioni tmux DI QUESTO utente.
  --user root             solo con --i-accept-root o conferma interattiva (ADR 0409 D5)
  --no-auto-update        aggiornamenti automatici spenti su questo host (ADR 0409 D6)
  --no-catalog            installazione dal catalogo spenta su questo host (ADR 0409 D7)
  --replace               sostituisce l'host già registrato su questa macchina (register -replace),
                          solo nello stesso spazio di lavoro (ADR 0409 A2)
  --server URL            istanza diversa da https://connect.monsterterminal.com (prove)
  --releases-url URL      base https da cui scaricare il rilascio, al posto di GitHub (banchi di
                          prova); la firma resta verificata con la chiave scritta nello script
  --version X.Y.Z         versione da installare (default: quella scritta nello script)
  --insecure-skip-signature  NON verifica la firma dei rilasci: solo per prove, mai in produzione
  --uninstall [--purge]   rimuove servizio, binario e configurazione (--purge anche stato e token)
  --dry-run               stampa i passi e non tocca nulla
USAGE
}

# ---------- funzioni pure (coperte da install_test.sh) ----------

# Utente dell'installazione esistente, dalla unit systemd ($1): il valore di User=, «root» se la unit
# non lo porta, niente se la unit non esiste. Un solo connettore per server (ADR 0409 A2).
unit_user() {
	[ -f "$1" ] || return 0
	u=$(sed -n 's/^User=//p' "$1" | head -n 1)
	printf '%s\n' "${u:-root}"
}

# Azione dopo `register -check` (ADR 0409 A2): $1 codice di uscita, $2 stdout, $3 --replace (yes|no).
# update | replace | replace-new | stop-not-registered | stop-other-tenant | stop-unavailable | stop-error
check_action() {
	case "$1:$2:$3" in
	"0:same-tenant same-machine:no" | "0:same-tenant other-machine:no") echo update ;;
	"0:same-tenant same-machine:yes") echo replace ;;
	"0:same-tenant other-machine:yes" | "0:not-registered:yes") echo replace-new ;;
	"0:not-registered:no") echo stop-not-registered ;;
	4:*) echo stop-other-tenant ;;
	5:*) echo stop-unavailable ;;
	*) echo stop-error ;;
	esac
}

# Il token ha la forma dei token di installazione del backend (connector-tokens.ts): mti_ + base64url.
valid_install_token() {
	case "$1" in
	mti_*) ;;
	*) return 1 ;;
	esac
	rest=${1#mti_}
	[ ${#rest} -ge 16 ] || return 1
	case "$rest" in
	*[!A-Za-z0-9_-]*) return 1 ;;
	esac
	return 0
}

# uname -m → architettura dei rilasci (common.json#arch).
map_arch() {
	case "$1" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) return 1 ;;
	esac
}

# Suggerimento per installare tmux, dalla famiglia di /etc/os-release (ID e ID_LIKE).
tmux_hint() {
	case " $1 " in
	*debian* | *ubuntu*) echo "apt-get install -y tmux" ;;
	*rhel* | *fedora* | *centos* | *rocky* | *alma*) echo "dnf install -y tmux" ;;
	*) echo "installa tmux col gestore dei pacchetti della distribuzione" ;;
	esac
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--user)
			[ $# -ge 2 ] || usage_die "--user vuole un nome"
			MT_USER=$2
			MT_USER_SET=yes
			shift
			;;
		--user=*)
			MT_USER=${1#--user=}
			MT_USER_SET=yes
			;;
		--i-accept-root) MT_ACCEPT_ROOT=yes ;;
		--no-auto-update) MT_AUTO_UPDATE=no ;;
		--no-catalog) MT_CATALOG=no ;;
		--replace) MT_REPLACE=yes ;;
		--server)
			[ $# -ge 2 ] || usage_die "--server vuole un URL"
			MT_SERVER=$2
			shift
			;;
		--server=*) MT_SERVER=${1#--server=} ;;
		--releases-url)
			[ $# -ge 2 ] || usage_die "--releases-url vuole un URL"
			MT_RELEASES=$2
			shift
			;;
		--releases-url=*) MT_RELEASES=${1#--releases-url=} ;;
		--version)
			[ $# -ge 2 ] || usage_die "--version vuole X.Y.Z"
			MT_WANT_VERSION=$2
			shift
			;;
		--version=*) MT_WANT_VERSION=${1#--version=} ;;
		--insecure-skip-signature) MT_SKIP_SIGNATURE=yes ;;
		--uninstall) MT_UNINSTALL=yes ;;
		--purge) MT_PURGE=yes ;;
		--dry-run) MT_DRY_RUN=yes ;;
		-h | --help)
			usage
			exit 0
			;;
		*) usage_die "opzione sconosciuta: $1" ;;
		esac
		shift
	done
	case "$MT_USER" in
	'' | *[!a-z0-9_-]* | -*) usage_die "nome utente non valido: «$MT_USER»" ;;
	esac
	case "$MT_SERVER" in
	https://*) ;;
	*) usage_die "--server deve essere https://…" ;;
	esac
	MT_RELEASES=${MT_RELEASES%/}
	case "$MT_RELEASES" in
	https://*) ;;
	*) usage_die "--releases-url deve essere https://…" ;;
	esac
	if [ -n "$MT_WANT_VERSION" ]; then
		case "$MT_WANT_VERSION" in
		*[!0-9.A-Za-z-]* | '') usage_die "--version non valida" ;;
		esac
		MT_VERSION=$MT_WANT_VERSION
	fi
	case "$MT_VERSION" in
	@*) [ "$MT_UNINSTALL" = yes ] || usage_die "questo script non è un rilascio: indica --version X.Y.Z" ;;
	esac
}

# La unit systemd. Nessun token nell'ambiente: il connettore legge il file 0600 nella cartella di stato.
# KillMode=process e NIENTE PrivateTmp/NoNewPrivileges: le sessioni tmux create dal connettore vivono
# nel suo gruppo di processi e devono sopravvivere a stop e riavvio (ADR 0409 D4), vedere il socket
# tmux dell'utente in /tmp e poter usare sudo come da una shell normale.
render_unit() {
	user=$1 home=$2 server=$3
	printf '%s\n' \
		"[Unit]" \
		"Description=MonsterTerminal connector" \
		"Documentation=https://github.com/monsterterminal/connector" \
		"After=network-online.target" \
		"Wants=network-online.target" \
		"" \
		"[Service]" \
		"Type=simple"
	if [ "$user" != root ]; then
		printf '%s\n' "User=$user" "Group=$user"
	fi
	printf '%s\n' \
		"Environment=HOME=$home" \
		"ExecStart=$MT_BIN run -server $server -token-file $MT_TOKEN_FILE -state-dir $MT_STATE" \
		"Restart=on-failure" \
		"RestartSec=5s" \
		"RestartPreventExitStatus=3" \
		"KillMode=process" \
		"UMask=0077" \
		"ProtectSystem=full" \
		"ProtectKernelTunables=yes" \
		"ProtectControlGroups=yes" \
		"ReadWritePaths=$MT_STATE" \
		"" \
		"[Install]" \
		"WantedBy=multi-user.target"
}

# La configurazione decisa dal cliente sull'host (§1.11): il backend non la cambia mai.
render_conf() {
	printf '%s\n' \
		"# MonsterTerminal connector — impostazioni decise dal cliente su questo host (§1.11)." \
		"# Cambiarle qui e riavviare il servizio: il backend non le può modificare." \
		"user=$1" \
		"auto_update=$2" \
		"catalog_install=$3" \
		"server=$4" \
		"installed_version=$5"
}

# OpenSSL verifica una firma Ed25519 grezza solo dalla 3.0 (`pkeyutl -rawin`): la 1.1.1 di Ubuntu
# 20.04 e Debian 11 conosce Ed25519 ma rifiuta l'opzione, e la verifica fallirebbe con un falso
# «firma non valida». Si chiede la capacità, non il numero di versione.
openssl_rawin_ok() {
	openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'
}

# Verifica Ed25519 grezza con OpenSSL (≥ 3.0): la firma staccata è i 64 byte di SHA256SUMS.sig.
verify_signature() {
	pub=$1 file=$2 sig=$3
	[ -s "$pub" ] || return 1
	[ -s "$sig" ] || return 1
	openssl pkeyutl -verify -pubin -inkey "$pub" -rawin -in "$file" -sigfile "$sig" >/dev/null 2>&1
}

# Lo SHA-256 del file deve comparire in SHA256SUMS (la riga esatta, non un altro asset).
verify_sha256() {
	sums=$1 dir=$2 name=$3
	grep -q "  $name\$" "$sums" || return 1
	(cd "$dir" && grep "  $name\$" "$sums" | sha256sum -c --quiet --status -)
}

# ---------- passi con effetti ----------

run() {
	if [ "$MT_DRY_RUN" = yes ]; then
		msg "  [dry-run] $*"
	else
		"$@"
	fi
}

require_root() {
	[ "$(id -u)" = 0 ] || die "serve root: lancia con sudo (il comando della UI lo fa già)"
}

check_tools() {
	command -v systemctl >/dev/null 2>&1 || die "serve systemd (systemctl non trovato)"
	[ -d /run/systemd/system ] || die "systemd non è il gestore dei servizi di questa macchina"
	command -v sha256sum >/dev/null 2>&1 || die "serve sha256sum (coreutils)"
	if command -v curl >/dev/null 2>&1; then
		MT_FETCH=curl
	elif command -v wget >/dev/null 2>&1; then
		MT_FETCH=wget
	else
		die "serve curl o wget"
	fi
	if [ "$MT_SKIP_SIGNATURE" = no ]; then
		command -v openssl >/dev/null 2>&1 || die "serve openssl (3.0 o successivo) per verificare la firma del rilascio"
		openssl_rawin_ok \
			|| die "questo openssl ($(openssl version 2>/dev/null)) non verifica firme Ed25519 grezze: serve OpenSSL 3.0 o successivo (Ubuntu 22.04+, Debian 12+, RHEL/Alma/Rocky 9+)"
		[ -n "$MT_UPDATES_PUBKEY_PEM" ] \
			|| die "questo script non porta la chiave pubblica dei rilasci: non può verificare nulla (vedi SECURITY.md)"
		# La chiave scritta qui deve avere l'impronta dichiarata: una copia corrotta o ritoccata a metà
		# non installa niente.
		fp=$(printf '%s\n' "$MT_UPDATES_PUBKEY_PEM" | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
		[ "$fp" = "$MT_UPDATES_PUBKEY_FINGERPRINT" ] \
			|| die "la chiave pubblica in questo script non ha l'impronta attesa ($MT_UPDATES_PUBKEY_FINGERPRINT): copia corrotta, non installo"
	else
		msg "ATTENZIONE: --insecure-skip-signature, la firma del rilascio NON viene verificata."
	fi
	if ! command -v tmux >/dev/null 2>&1; then
		# shellcheck disable=SC1091
		like=$(. /etc/os-release 2>/dev/null && printf '%s %s' "${ID:-}" "${ID_LIKE:-}")
		msg "tmux non trovato: il terminale lo richiede. Installalo con: $(tmux_hint "$like")"
	fi
}

read_token() {
	MT_TOKEN=${MONSTERTERMINAL_INSTALL_TOKEN:-}
	unset MONSTERTERMINAL_INSTALL_TOKEN
	[ -n "$MT_TOKEN" ] || die "manca MONSTERTERMINAL_INSTALL_TOKEN: copia il comando completo dalla UI"
	valid_install_token "$MT_TOKEN" || die "il token di installazione non ha la forma attesa (mti_…)"
}

confirm_root() {
	[ "$MT_USER" = root ] || return 0
	[ "$MT_ACCEPT_ROOT" = yes ] && return 0
	msg "Hai scelto di far girare il connettore come ROOT: chi apre un terminale dalla UI di"
	msg "MonsterTerminal avrà una shell di root su questa macchina (ADR 0409 D5)."
	if [ -r /dev/tty ]; then
		printf 'Scrivi ROOT per confermare: ' >&2
		read -r answer </dev/tty || answer=""
		[ "$answer" = ROOT ] || die "non confermato: scegli un altro utente con --user"
	else
		die "senza terminale la scelta di root richiede --i-accept-root"
	fi
}

# Server già registrato (token dell'host presente): prima di toccare qualunque cosa, un solo
# connettore per server e la verifica non consumante dello spazio di lavoro col binario appena
# verificato (ADR 0409 A2). Il token di installazione passa sullo stdin del solo processo.
check_existing() {
	[ -s "$MT_TOKEN_FILE" ] || return 0
	existing=$(unit_user "$MT_UNIT")
	if [ -n "$existing" ]; then
		if [ "$MT_USER_SET" = yes ] && [ "$MT_USER" != "$existing" ]; then
			die "su questo server il connettore è già installato per l'utente $existing: ce ne può essere uno solo. Non modifico nulla. Per cambiare utente: sh install.sh --uninstall, poi revoca l'host dalla UI e reinstalla."
		fi
		MT_USER=$existing
	fi
	msg "Server già registrato: verifico a quale spazio di lavoro appartiene…"
	chmod 0700 "$MT_TMP/$MT_ASSET"
	rc=0
	out=$(printf '%s\n' "$MT_TOKEN" | env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin "$MT_TMP/$MT_ASSET" register -check \
		-server "$MT_SERVER" -token-file "$MT_TOKEN_FILE" -state-dir "$MT_STATE" 2>"$MT_TMP/check.err") || rc=$?
	case $(check_action "$rc" "$out" "$MT_REPLACE") in
	update) msg "Server già registrato per questo spazio di lavoro: aggiorno senza registrare di nuovo." ;;
	replace) msg "Sostituisco l'host di questo server: il precedente sarà revocato." ;;
	replace-new) msg "Registro un host nuovo per questo server; l'eventuale registrazione precedente non viene revocata." ;;
	stop-not-registered)
		die "la registrazione di questo server non è più valida (host revocato): non modifico nulla. Rilancia con --replace e un token nuovo."
		;;
	stop-other-tenant)
		die "questo server è già registrato per un altro spazio di lavoro: non modifico nulla. Per spostarlo, revocalo da quello spazio e rilancia con --replace."
		;;
	stop-unavailable)
		die "il servizio di MonsterTerminal non può ancora verificare a quale spazio di lavoro appartiene questo server: non modifico nulla. Riprova più tardi o scrivi a support@monsterterminal.com."
		;;
	*)
		die "verifica dello spazio di lavoro non riuscita: non modifico nulla. $(tail -n 1 "$MT_TMP/check.err" 2>/dev/null)"
		;;
	esac
}

ensure_user() {
	if [ "$MT_USER" = root ]; then
		MT_HOME=/root
		return 0
	fi
	if getent passwd "$MT_USER" >/dev/null 2>&1; then
		MT_HOME=$(getent passwd "$MT_USER" | cut -d: -f6)
		[ -n "$MT_HOME" ] || die "l'utente $MT_USER non ha una home"
		msg "Utente esistente: $MT_USER (home $MT_HOME)"
		return 0
	fi
	[ "$MT_USER" = "$MT_DEFAULT_USER" ] \
		|| die "l'utente $MT_USER non esiste: crealo prima, oppure ometti --user per l'utente dedicato"
	MT_HOME=/home/$MT_DEFAULT_USER
	msg "Creo l'utente dedicato $MT_USER (home $MT_HOME, shell /bin/bash)"
	run useradd --system --create-home --home-dir "$MT_HOME" --shell /bin/bash "$MT_USER"
}

fetch() {
	url=$1 dest=$2
	if [ "$MT_FETCH" = curl ]; then
		curl -fsSL --proto '=https' --tlsv1.2 -o "$dest" "$url"
	else
		wget -q --https-only -O "$dest" "$url"
	fi
}

download_release() {
	arch=$(map_arch "$(uname -m)") || die "architettura non supportata: $(uname -m) (serve x86_64 o aarch64)"
	MT_ASSET="monsterterminal-connector_${MT_VERSION}_linux_${arch}"
	base="$MT_RELEASES/v$MT_VERSION"
	MT_TMP=$(mktemp -d) || die "mktemp fallito"
	chmod 0700 "$MT_TMP"
	trap 'rm -rf "$MT_TMP"' EXIT INT TERM
	if [ "$MT_RELEASES" = "$MT_DEFAULT_RELEASES" ]; then
		msg "Scarico il rilascio v$MT_VERSION ($arch)…"
	else
		msg "Scarico il rilascio v$MT_VERSION ($arch) da $MT_RELEASES (non da GitHub)…"
	fi
	fetch "$base/SHA256SUMS" "$MT_TMP/SHA256SUMS" || die "SHA256SUMS non scaricabile da $base"
	if [ "$MT_SKIP_SIGNATURE" = no ]; then
		fetch "$base/SHA256SUMS.sig" "$MT_TMP/SHA256SUMS.sig" || die "firma SHA256SUMS.sig non scaricabile"
		printf '%s\n' "$MT_UPDATES_PUBKEY_PEM" >"$MT_TMP/updates.pub"
		verify_signature "$MT_TMP/updates.pub" "$MT_TMP/SHA256SUMS" "$MT_TMP/SHA256SUMS.sig" \
			|| die "la firma del rilascio NON è valida: non installo nulla (§1.10)"
		msg "Firma del rilascio verificata."
	fi
	fetch "$base/$MT_ASSET" "$MT_TMP/$MT_ASSET" || die "binario $MT_ASSET non scaricabile"
	verify_sha256 "$MT_TMP/SHA256SUMS" "$MT_TMP" "$MT_ASSET" \
		|| die "lo SHA-256 del binario non corrisponde a SHA256SUMS: non installo nulla"
	msg "Integrità del binario verificata."
}

install_files() {
	msg "Installo $MT_BIN e le cartelle"
	run install -d -m 0755 -o root -g root "$MT_ETC"
	run install -d -m 0700 -o "$MT_USER" -g "$MT_USER" "$MT_STATE"
	if [ "$MT_DRY_RUN" = yes ]; then
		msg "  [dry-run] install -m 0755 $MT_ASSET $MT_BIN"
		return 0
	fi
	install -m 0755 -o root -g root "$MT_TMP/$MT_ASSET" "$MT_BIN.new"
	mv -f "$MT_BIN.new" "$MT_BIN"
	render_conf "$MT_USER" "$MT_AUTO_UPDATE" "$MT_CATALOG" "$MT_SERVER" "$MT_VERSION" >"$MT_CONF.new"
	chmod 0644 "$MT_CONF.new"
	mv -f "$MT_CONF.new" "$MT_CONF"
}

# Registrazione come l'utente del connettore. Il token passa sullo standard input del solo processo
# register (la pipe, non lo stdin di sh, che è lo script stesso); env -i toglie l'ambiente di root.
register_host() {
	# Già registrato e nessun --replace: check_existing ha verificato lo spazio e lo ha già detto.
	if [ -s "$MT_TOKEN_FILE" ] && [ "$MT_REPLACE" = no ]; then
		return 0
	fi
	set -- "$MT_BIN" register -server "$MT_SERVER" -token-file "$MT_TOKEN_FILE" -state-dir "$MT_STATE"
	[ "$MT_REPLACE" = yes ] && set -- "$@" -replace
	msg "Registro l'host come $MT_USER…"
	if [ "$MT_DRY_RUN" = yes ]; then
		msg "  [dry-run] runuser -u $MT_USER -- env -i HOME=$MT_HOME $*"
		return 0
	fi
	if [ "$MT_USER" = root ]; then
		printf '%s\n' "$MT_TOKEN" | env -i HOME="$MT_HOME" PATH=/usr/local/bin:/usr/bin:/bin "$@"
	else
		printf '%s\n' "$MT_TOKEN" | runuser -u "$MT_USER" -- env -i HOME="$MT_HOME" PATH=/usr/local/bin:/usr/bin:/bin USER="$MT_USER" "$@"
	fi || die "registrazione fallita: il messaggio sopra dice cosa fare (il token non è consumato su 400/403)"
}

install_service() {
	msg "Installo il servizio $MT_UNIT_NAME"
	if [ "$MT_DRY_RUN" = yes ]; then
		render_unit "$MT_USER" "$MT_HOME" "$MT_SERVER" | sed 's/^/  [dry-run] /' >&2
		return 0
	fi
	render_unit "$MT_USER" "$MT_HOME" "$MT_SERVER" >"$MT_UNIT.new"
	chmod 0644 "$MT_UNIT.new"
	mv -f "$MT_UNIT.new" "$MT_UNIT"
	systemctl daemon-reload
	systemctl enable --now "$MT_UNIT_NAME" >/dev/null 2>&1 || systemctl restart "$MT_UNIT_NAME"
	i=0
	while [ $i -lt 30 ]; do
		systemctl is-active --quiet "$MT_UNIT_NAME" && break
		i=$((i + 1))
		sleep 1
	done
	systemctl is-active --quiet "$MT_UNIT_NAME" || {
		journalctl -u "$MT_UNIT_NAME" -n 20 --no-pager >&2 || true
		die "il servizio non è attivo: vedi sopra"
	}
}

summary() {
	msg ""
	msg "MonsterTerminal connector v$MT_VERSION installato."
	msg "  utente:          $MT_USER"
	msg "  aggiornamenti:   $MT_AUTO_UPDATE   catalogo: $MT_CATALOG   ($MT_CONF)"
	msg "  servizio:        systemctl status $MT_UNIT_NAME    log: journalctl -u $MT_UNIT_NAME -f"
	msg "  disinstallare:   sh install.sh --uninstall   (poi revoca l'host dalla UI)"
}

uninstall() {
	require_root
	msg "Rimuovo il servizio e il binario (l'host resta registrato: revocalo dalla UI di MonsterTerminal)"
	if [ -f "$MT_UNIT" ]; then
		run systemctl disable --now "$MT_UNIT_NAME"
		run rm -f "$MT_UNIT"
		run systemctl daemon-reload
	fi
	run rm -f "$MT_BIN"
	run rm -rf "$MT_ETC"
	if [ "$MT_PURGE" = yes ]; then
		run rm -rf "$MT_STATE"
	else
		msg "Stato e token restano in $MT_STATE (--purge per rimuoverli)"
	fi
	# L'utente non si tocca: lo script non sa se l'ha creato lui o era del cliente (--user), e con
	# lui andrebbe via la home con le sessioni tmux. Chi lo vuole via lo fa a mano.
	if [ "$MT_USER" != root ] && getent passwd "$MT_USER" >/dev/null 2>&1; then
		msg "L'utente $MT_USER resta (con la sua home): se l'aveva creato questo script, userdel -r $MT_USER"
	fi
}

main() {
	parse_args "$@"
	if [ "$MT_UNINSTALL" = yes ]; then
		uninstall
		exit 0
	fi
	require_root
	check_tools
	read_token
	confirm_root
	# Prima il rilascio verificato, poi l'utente: con una firma non valida non deve restare nulla
	# sulla macchina, nemmeno un utente (rilievo del banco 2, 20/10).
	download_release
	check_existing
	ensure_user
	install_files
	register_host
	install_service
	summary
}

# Caricato dal banco di prova con MT_INSTALL_SOURCED=1: definisce le funzioni e non esegue nulla.
if [ "${MT_INSTALL_SOURCED:-}" = 1 ]; then
	# shellcheck disable=SC2317
	return 0 2>/dev/null || exit 0
fi
main "$@"
