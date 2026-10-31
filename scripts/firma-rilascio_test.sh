#!/bin/sh
# Banco di scripts/firma-rilascio.sh, senza rete e senza la chiave vera: un `gh` finto nel PATH serve
# una bozza preparata qui, una coppia Ed25519 cifrata nasce al momento e l'impronta attesa dello script
# è sostituita, in una copia, con la sua. Il terminale lo dà python3 (pty). Corrispondenza (#26):
# spec §1.17 e §1.19 punto 3 (si firma solo una bozza, solo con mt-updates-2026, ogni firma verificata
# prima di caricarla), richiesta BOSS 2026-10-31 (un comando, passphrase chiesta una volta a video).
# La ricompilazione (passo 2) qui si salta con --no-rebuild: la prova la release vera, che fallisce se
# gli hash non tornano.
#
#   sh scripts/firma-rilascio_test.sh
#
# Le prove sono stringhe passate a eval dentro check, come in packaging/install_test.sh.
# shellcheck disable=SC2016
set -eu
here=$(cd "$(dirname "$0")" && pwd)
fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
PASSPHRASE='prova di banco: 12 parole'
openssl genpkey -algorithm ed25519 -aes-256-cbc -pass "pass:$PASSPHRASE" -out "$tmp/k.key" 2>/dev/null
openssl genpkey -algorithm ed25519 -aes-256-cbc -pass "pass:$PASSPHRASE" -out "$tmp/altra.key" 2>/dev/null
fp=$(openssl pkey -in "$tmp/k.key" -passin "pass:$PASSPHRASE" -pubout -outform DER | sha256sum | cut -d' ' -f1)
openssl pkey -in "$tmp/k.key" -passin "pass:$PASSPHRASE" -pubout -out "$tmp/k.pub"
sed "s/^UPDATES_FINGERPRINT=.*/UPDATES_FINGERPRINT=$fp/" "$here/firma-rilascio.sh" >"$tmp/firma.sh"

# La bozza: due binari, install.sh, un manifest, SHA256SUMS coerente.
mkdir -p "$tmp/draft" "$tmp/bin"
for f in monsterterminal-connector_9.9.9_linux_amd64 monsterterminal-connector_9.9.9_linux_arm64 install.sh update-manifest-amd64.json; do
	printf 'contenuto di %s\n' "$f" >"$tmp/draft/$f"
done
(cd "$tmp/draft" && sha256sum monsterterminal-connector_* install.sh update-manifest-amd64.json >SHA256SUMS)

cat >"$tmp/bin/gh" <<'GH'
#!/bin/sh
# gh finto: le azioni che firma-rilascio usa, sullo stato in $GH_STATE.
case "$1 $2" in
"auth status") exit 0 ;;
"release view") cat "$GH_STATE/draft-flag" ;;
"release download")
	while [ $# -gt 0 ]; do [ "$1" = --dir ] && dir=$2; shift; done
	cp -r "$GH_STATE/assets" "$dir" ;;
"release upload")
	mkdir -p "$GH_STATE/uploaded"
	for a in "$@"; do case "$a" in *.sig) cp "$a" "$GH_STATE/uploaded/" ;; esac; done ;;
"release edit") echo "$*" >>"$GH_STATE/edits" ;;
*) echo "gh finto: $*" >&2; exit 2 ;;
esac
GH
chmod +x "$tmp/bin/gh"

# corsa NOME RISPOSTE ARGS…: lancia lo script in un terminale vero; RISPOSTE sono le righe date ai
# prompt, in ordine (passphrase, poi conferma). Scrive uscita, trascrizione e numero di prompt.
corsa() {
	name=$1 answers=$2
	shift 2
	st="$tmp/st-$name"
	rm -rf "$st" && mkdir -p "$st" && cp -r "$tmp/draft" "$st/assets"
	[ -e "$st/draft-flag" ] || echo true >"$st/draft-flag"
	[ -z "${DRAFT:-}" ] || echo "$DRAFT" >"$st/draft-flag"
	[ -z "${CORROMPI:-}" ] || printf 'manomesso\n' >>"$st/assets/install.sh"
	GH_STATE=$st PATH="$tmp/bin:$PATH" HOME="$tmp/home" python3 - "$st" "$answers" sh "$tmp/firma.sh" "$@" <<'PY'
import os, pty, sys, select
st, answers, argv = sys.argv[1], sys.argv[2].split("|") if sys.argv[2] else [], sys.argv[3:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(argv[0], argv)
out, seen = b"", {b"Passphrase di ": 0, b"Pubblico la release ": 0}
while True:
    r, _, _ = select.select([fd], [], [], 20)
    if not r:
        os.kill(pid, 9)  # fermo su un prompt che il banco non conosce: meglio rosso che appeso
        break
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out += chunk
    for marker in seen:
        while out.count(marker) > seen[marker]:
            seen[marker] += 1
            os.write(fd, ((answers.pop(0) if answers else "") + "\n").encode())
prompts = sum(seen.values())
_, status = os.waitpid(pid, 0)
open(os.path.join(st, "out"), "wb").write(out)
open(os.path.join(st, "rc"), "w").write(str(os.waitstatus_to_exitcode(status)))
open(os.path.join(st, "prompts"), "w").write(str(prompts))
PY
}
rc() { cat "$tmp/st-$1/rc"; }
out() { cat "$tmp/st-$1/out"; }
mkdir -p "$tmp/home"

corsa ok "$PASSPHRASE|s" 9.9.9 --key "$tmp/k.key" --repo prova/connettore --no-rebuild
check "firma e pubblica: uscita 0" '[ "$(rc ok)" = 0 ]'
check "la passphrase è chiesta UNA volta, più la conferma" '[ "$(out ok | grep -c "Passphrase di ")" = 1 ] && [ "$(cat "$tmp/st-ok/prompts")" = 2 ]'
check "tre firme caricate (SHA256SUMS, install.sh, manifest)" '[ "$(ls "$tmp/st-ok/uploaded" | wc -l)" = 3 ]'
check "ogni firma si verifica con la chiave pubblica" 'for f in SHA256SUMS install.sh update-manifest-amd64.json; do openssl pkeyutl -verify -pubin -inkey "$tmp/k.pub" -rawin -in "$tmp/draft/$f" -sigfile "$tmp/st-ok/uploaded/$f.sig" >/dev/null || exit 1; done'
check "release tolta dalla bozza" 'grep -q -- "--draft=false" "$tmp/st-ok/edits"'
check "la passphrase non compare nella trascrizione" '! out ok | grep -qF "$PASSPHRASE"'
check "v davanti alla versione aggiunta da sé" 'out ok | grep -q "bozza v9.9.9"'

corsa no "$PASSPHRASE|n" 9.9.9 --key "$tmp/k.key" --repo prova/connettore --no-rebuild
check "conferma negata: firme caricate, release ancora in bozza" '[ "$(rc no)" = 0 ] && [ "$(ls "$tmp/st-no/uploaded" | wc -l)" = 3 ] && [ ! -e "$tmp/st-no/edits" ]'

corsa sbagliata "non è questa" 9.9.9 --key "$tmp/k.key" --repo prova/connettore --no-rebuild
check "passphrase sbagliata: si ferma, niente caricato" '[ "$(rc sbagliata)" != 0 ] && [ ! -e "$tmp/st-sbagliata/uploaded" ] && out sbagliata | grep -q "passphrase sbagliata"'

corsa altra "$PASSPHRASE" 9.9.9 --key "$tmp/altra.key" --repo prova/connettore --no-rebuild
check "chiave che non è mt-updates-2026: si ferma, niente caricato" '[ "$(rc altra)" != 0 ] && [ ! -e "$tmp/st-altra/uploaded" ] && out altra | grep -q "non è mt-updates-2026"'

DRAFT=false corsa pubblica "" 9.9.9 --key "$tmp/k.key" --repo prova/connettore --no-rebuild
check "release già pubblica: si ferma prima di chiedere la passphrase" '[ "$(rc pubblica)" != 0 ] && [ "$(cat "$tmp/st-pubblica/prompts")" = 0 ] && out pubblica | grep -q "non è una bozza"'

CORROMPI=1 corsa corrotta "" 9.9.9 --key "$tmp/k.key" --repo prova/connettore --no-rebuild
check "asset diverso da SHA256SUMS: si ferma prima della passphrase" '[ "$(rc corrotta)" != 0 ] && [ "$(cat "$tmp/st-corrotta/prompts")" = 0 ] && [ ! -e "$tmp/st-corrotta/uploaded" ]'

corsa versione "" 9.9 --key "$tmp/k.key" --repo prova/connettore --no-rebuild
check "versione malformata rifiutata" '[ "$(rc versione)" != 0 ] && out versione | grep -q "versione non valida"'

check "senza terminale rifiuta (la passphrase va chiesta a video)" '! PATH="$tmp/bin:$PATH" sh "$tmp/firma.sh" 9.9.9 --key "$tmp/k.key" --no-rebuild </dev/null 2>/dev/null'

if [ "$fails" -gt 0 ]; then
	printf '%d prove ROSSE\n' "$fails"
	exit 1
fi
printf 'firma-rilascio: tutte le prove verdi\n'
