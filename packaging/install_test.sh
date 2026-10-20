#!/bin/sh
# Banco di prova di install.sh: carica lo script con MT_INSTALL_SOURCED=1 e prova le funzioni pure e
# il --dry-run, senza root e senza toccare la macchina. Corrispondenza (regola #26): spec §1.14 (utente
# esistente / dedicato / root con conferma; servizio systemd; token mai come argomento), §1.10 e §1.17
# (firma Ed25519 non valida ⇒ nessuna installazione), ADR 0409 D5, D6, D7.
#
#   sh packaging/install_test.sh      (anche con dash: dash packaging/install_test.sh)
#
# Le prove sono stringhe passate a eval dentro check: per shellcheck le variabili sembrano inutilizzate
# e le espansioni fra apici singoli sbagliate, ma è la forma voluta del banco.
# shellcheck disable=SC2016,SC2034,SC1091
set -eu
here=$(cd "$(dirname "$0")" && pwd)
fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

MT_INSTALL_SOURCED=1
. "$here/install.sh"

# --- token (§1.3, connector-tokens.ts) ---
check "token valido" 'valid_install_token mti_abcdefghijklmnop0123'
check "token con prefisso sbagliato" '! valid_install_token mth_abcdefghijklmnop0123'
check "token troppo corto" '! valid_install_token mti_abc'
check "token con caratteri estranei" '! valid_install_token "mti_abcdefghijklmnop;rm -rf"'

# --- architetture (common.json#arch) ---
check "x86_64 → amd64" '[ "$(map_arch x86_64)" = amd64 ]'
check "aarch64 → arm64" '[ "$(map_arch aarch64)" = arm64 ]'
check "armv7l rifiutata" '! map_arch armv7l'
check "hint tmux debian" '[ "$(tmux_hint "ubuntu debian")" = "apt-get install -y tmux" ]'
check "hint tmux rhel" '[ "$(tmux_hint "rocky rhel centos fedora")" = "dnf install -y tmux" ]'

# --- opzioni (§1.14, D5-D7) ---
MT_VERSION=0.2.0
parse_args
check "default: utente dedicato" '[ "$MT_USER" = monsterterminal ]'
check "default: aggiornamenti e catalogo accesi" '[ "$MT_AUTO_UPDATE" = yes ] && [ "$MT_CATALOG" = yes ]'
parse_args --user mario --no-auto-update --no-catalog --replace --server https://prova.example
check "--user esistente" '[ "$MT_USER" = mario ]'
check "--no-auto-update (D6)" '[ "$MT_AUTO_UPDATE" = no ]'
check "--no-catalog (D7)" '[ "$MT_CATALOG" = no ]'
check "--replace" '[ "$MT_REPLACE" = yes ]'
check "--server" '[ "$MT_SERVER" = https://prova.example ]'
check "--server http rifiutato" '! (parse_args --server http://prova.example) 2>/dev/null'
check "senza --releases-url resta GitHub" '[ "$MT_RELEASES" = https://github.com/monsterterminal/connector/releases/download ]'
parse_args --releases-url https://banco.example/rilasci/
check "--releases-url, senza la barra finale" '[ "$MT_RELEASES" = https://banco.example/rilasci ]'
check "--releases-url http rifiutato" '! (parse_args --releases-url http://banco.example) 2>/dev/null'
MT_RELEASES=$MT_DEFAULT_RELEASES
check "utente con caratteri estranei rifiutato" '! (parse_args --user "ma rio") 2>/dev/null'
check "opzione sconosciuta ⇒ uscita 2" '(parse_args --boh) 2>/dev/null; [ $? = 2 ]'
MT_VERSION="@VERSION@"
check "script non rilasciato senza --version rifiutato" '! (parse_args) 2>/dev/null'
check "script non rilasciato con --version accettato" '(parse_args --version 0.2.0) 2>/dev/null'
MT_VERSION=0.2.0

# --- root: conferma esplicita (D5) ---
MT_USER=root MT_ACCEPT_ROOT=no
check "root senza terminale e senza flag rifiutato" '! (confirm_root </dev/null) 2>/dev/null'
MT_ACCEPT_ROOT=yes
check "root con --i-accept-root accettato" 'confirm_root'
MT_USER=monsterterminal MT_ACCEPT_ROOT=no

# --- unit systemd (§1.14: servizio systemd; token mai nell'ambiente) ---
unit=$(render_unit mario /home/mario https://connect.monsterterminal.com)
check "unit: User e Group" 'printf "%s" "$unit" | grep -q "^User=mario$" && printf "%s" "$unit" | grep -q "^Group=mario$"'
check "unit: ExecStart con token-file e state-dir" 'printf "%s" "$unit" | grep -q "^ExecStart=/usr/local/bin/monsterterminal-connector run -server https://connect.monsterterminal.com -token-file /var/lib/monsterterminal/host-token -state-dir /var/lib/monsterterminal$"'
check "unit: nessun token nell'ambiente" '! printf "%s" "$unit" | grep -qi "token=\|mti_"'
check "unit: uscita 3 non riavvia (host revocato)" 'printf "%s" "$unit" | grep -q "^RestartPreventExitStatus=3$"'
check "unit: le sessioni tmux sopravvivono (KillMode=process, D4)" 'printf "%s" "$unit" | grep -q "^KillMode=process$"'
check "unit: niente PrivateTmp (socket tmux condiviso)" '! printf "%s" "$unit" | grep -q "PrivateTmp"'
unitroot=$(render_unit root /root https://connect.monsterterminal.com)
check "unit root: senza User=" '! printf "%s" "$unitroot" | grep -q "^User="'

conf=$(render_conf mario no yes https://connect.monsterterminal.com 0.2.0)
check "conf: auto_update e catalog_install (§1.11)" 'printf "%s" "$conf" | grep -q "^auto_update=no$" && printf "%s" "$conf" | grep -q "^catalog_install=yes$"'

# --- firma Ed25519 con OpenSSL (§1.10, §1.17): una coppia generata qui, mai una chiave vera ---
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
openssl genpkey -algorithm ed25519 -out "$tmp/k.pem" 2>/dev/null
openssl pkey -in "$tmp/k.pem" -pubout -out "$tmp/k.pub" 2>/dev/null
printf 'ciao\n' >"$tmp/bin"
(cd "$tmp" && sha256sum bin >SHA256SUMS)
openssl pkeyutl -sign -inkey "$tmp/k.pem" -rawin -in "$tmp/SHA256SUMS" -out "$tmp/SHA256SUMS.sig" 2>/dev/null
check "firma valida verificata" 'verify_signature "$tmp/k.pub" "$tmp/SHA256SUMS" "$tmp/SHA256SUMS.sig"'
printf 'x' >>"$tmp/SHA256SUMS"
check "SHA256SUMS alterato ⇒ firma non valida" '! verify_signature "$tmp/k.pub" "$tmp/SHA256SUMS" "$tmp/SHA256SUMS.sig"'
(cd "$tmp" && sha256sum bin >SHA256SUMS)
printf '\0' | dd of="$tmp/SHA256SUMS.sig" bs=1 seek=3 count=1 conv=notrunc 2>/dev/null
check "firma corrotta ⇒ non valida" '! verify_signature "$tmp/k.pub" "$tmp/SHA256SUMS" "$tmp/SHA256SUMS.sig"'
openssl genpkey -algorithm ed25519 -out "$tmp/k2.pem" 2>/dev/null
openssl pkey -in "$tmp/k2.pem" -pubout -out "$tmp/k2.pub" 2>/dev/null
openssl pkeyutl -sign -inkey "$tmp/k.pem" -rawin -in "$tmp/SHA256SUMS" -out "$tmp/SHA256SUMS.sig" 2>/dev/null
check "firma di un'altra chiave ⇒ non valida" '! verify_signature "$tmp/k2.pub" "$tmp/SHA256SUMS" "$tmp/SHA256SUMS.sig"'
: >"$tmp/vuota.pub"
check "chiave pubblica assente ⇒ non valida" '! verify_signature "$tmp/vuota.pub" "$tmp/SHA256SUMS" "$tmp/SHA256SUMS.sig"'

# --- SHA-256 del binario ---
check "sha256 corretto" 'verify_sha256 "$tmp/SHA256SUMS" "$tmp" bin'
printf 'altro' >"$tmp/bin"
check "binario alterato ⇒ sha256 non corrisponde" '! verify_sha256 "$tmp/SHA256SUMS" "$tmp" bin'
check "asset assente da SHA256SUMS rifiutato" '! verify_sha256 "$tmp/SHA256SUMS" "$tmp" altro'

# --- la chiave scritta nello script è mt-updates-2026 (§1.17), la stessa di SECURITY.md ---
check "chiave pubblica nello script = impronta dichiarata" '[ "$(printf "%s\n" "$MT_UPDATES_PUBKEY_PEM" | openssl pkey -pubin -outform DER | sha256sum | cut -d" " -f1)" = "$MT_UPDATES_PUBKEY_FINGERPRINT" ]'
check "impronta dichiarata = 90efb21f2321…" '[ "$MT_UPDATES_PUBKEY_FINGERPRINT" = 90efb21f2321b1a530efff3973b5d8f4f354675af509958f3f08422cd1c43b0d ]'

# --- OpenSSL 1.1.1 (Ubuntu 20.04, Debian 11): niente -rawin, check_tools deve dirlo, non «firma non valida» ---
mkdir -p "$tmp/vecchio"
cat >"$tmp/vecchio/openssl" <<'OSSL'
#!/bin/sh
# openssl 1.1.1 finto: conosce pkeyutl ma non -rawin.
case "$1 $2" in
version*) echo "OpenSSL 1.1.1f  31 Mar 2020" ;;
"pkeyutl -help") printf '%s\n' "Usage: pkeyutl [options]" " -in infile" " -sigfile file" >&2 ;;
*) exec "$REAL_OPENSSL" "$@" ;;
esac
OSSL
chmod +x "$tmp/vecchio/openssl"
check "openssl vero: -rawin riconosciuto" 'openssl_rawin_ok'
REAL_OPENSSL=$(command -v openssl); export REAL_OPENSSL
check "openssl 1.1.1: -rawin non riconosciuto" '! (PATH="$tmp/vecchio:$PATH"; openssl_rawin_ok)'
MT_SKIP_SIGNATURE=no
oldmsg=$( (PATH="$tmp/vecchio:$PATH"; check_tools) 2>&1) || true
check "openssl 1.1.1: check_tools si ferma chiedendo OpenSSL 3" 'printf "%s" "$oldmsg" | grep -q "serve OpenSSL 3.0 o successivo"'

# --- check_tools: senza chiave pubblica nello script non si installa (§1.17) ---
MT_SKIP_SIGNATURE=no
check "con la chiave vera check_tools passa" '(check_tools) 2>/dev/null'
MT_UPDATES_PUBKEY_PEM=$(cat "$tmp/k.pub")
check "chiave diversa dall'impronta dichiarata ⇒ lo script si ferma" '! (check_tools) 2>/dev/null'
MT_UPDATES_PUBKEY_PEM=""
check "senza chiave pubblica lo script si ferma" '! (check_tools) 2>/dev/null'

# --- ordine dei passi: il rilascio si scarica e si verifica PRIMA di creare l'utente (banco 2) ---
check "main: download_release precede ensure_user" '[ "$(grep -n "^	download_release$" "$here/install.sh" | cut -d: -f1)" -lt "$(grep -n "^	ensure_user$" "$here/install.sh" | cut -d: -f1)" ]'

# --- --dry-run: nessun effetto, e il token non compare mai nell'uscita ---
# Lo script sorgente porta la chiave vera; `|| true` perché un die nel sottoshell deve far fallire le
# prove qui sotto a voce alta, non uscire dalla suite in silenzio (set -e).
out=$(MONSTERTERMINAL_INSTALL_TOKEN=mti_abcdefghijklmnop0123 sh -c '
	MT_INSTALL_SOURCED=1; . "$1"; MT_VERSION=0.2.0
	parse_args --dry-run --user "$(id -un)"
	id() { echo 0; }
	check_tools 2>/dev/null || true
	read_token; confirm_root; ensure_user
	MT_ASSET=finto; MT_TMP=$2
	install_files; register_host; install_service; summary
' sh "$here/install.sh" "$tmp" 2>&1) || true
check "dry-run elenca i passi" 'printf "%s" "$out" | grep -q "\[dry-run\] install -d" && printf "%s" "$out" | grep -q "\[dry-run\] runuser"'
check "dry-run non stampa il token" '! printf "%s" "$out" | grep -q "mti_"'
check "dry-run non scrive il binario" '[ ! -e /usr/local/bin/monsterterminal-connector.new ]'

printf '\n%s prove, %s fallite\n' "$(grep -c '^check ' "$0")" "$fails"
[ "$fails" = 0 ]
