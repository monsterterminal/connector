#!/bin/sh
# Firma di una ricetta del catalogo, in un solo comando, sul PC di chi custodisce la chiave (WSL sul PC
# Jack; mai sul server né su GitHub: spec §1.9, §1.17, §1.19, ADR 0410 A4 decisione 2). Gemello di
# firma-rilascio.sh con la chiave delle ricette mt-recipes-2026, MAI quella degli aggiornamenti.
#   1. valida la ricetta con scripts/valida-ricetta.sh (schema 0.11.0, forma canonica, scadenza entro
#      180 giorni, source coerente, niente latest né http): se non passa, non si firma;
#   2. chiede la passphrase UNA volta a video, controlla che la chiave sia mt-recipes-2026 (impronta),
#      firma i BYTE ESATTI del file (Ed25519 grezzo, 64 byte) e verifica subito la firma con la
#      chiave pubblica derivata;
#   3. scrive recipes/<recipeId>/<recipeVersion>.json.sig: la firma in base64 su una riga.
# La CI verifica ogni .sig con la chiave pubblica incorporata (--verifica); il super importa la coppia
# dal repository e il backend la riverifica prima di salvarla (§1.9).
#
#   firma-ricetta codex-amd64 1                        firma recipes/codex-amd64/1.json
#   firma-ricetta codex-amd64 1 --key PERCORSO         chiave diversa da ~/chiavi-monsterterminal/mt-recipes-2026.key
#   firma-ricetta codex-amd64 1 --sovrascrivi          sostituisce un .sig esistente
#   firma-ricetta codex-amd64 1 --verifica             controlla il .sig esistente con la chiave pubblica
#   firma-ricetta codex-amd64 1 --recipes-dir DIR      cartella delle ricette diversa da <repo>/recipes
#
# Serve: openssl ≥ 3.0 (pkeyutl -rawin), go (per la validazione). La passphrase non finisce mai in un
# argomento, in una variabile d'ambiente o su disco: passa a openssl sullo standard input.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
RECIPES_DIR=$root/recipes
KEY=$HOME/chiavi-monsterterminal/mt-recipes-2026.key
VERIFY=no
OVERWRITE=no
ID=""
VER=""
# La chiave pubblica mt-recipes-2026 (SECURITY.md, internal/signing/signing.go): con un'altra, per
# esempio quella degli aggiornamenti, la firma sarebbe perfetta e nessun host la accetterebbe.
RECIPES_PUB_B64=MCowBQYDK2VwAyEAzdEkzqbCqbjnd5kDmEzdCtjuXXbEJZNXDZXQbsHS6Zs=
RECIPES_FINGERPRINT=12fd1e92f867a787abb5a18d4b07d8cbaaac3363c10bd921cadce06bb85bbd54

die() { printf 'firma-ricetta: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--key) [ $# -ge 2 ] || die "--key vuole un percorso"; KEY=$2; shift ;;
	--recipes-dir) [ $# -ge 2 ] || die "--recipes-dir vuole una cartella"; RECIPES_DIR=$2; shift ;;
	--verifica) VERIFY=yes ;;
	--sovrascrivi) OVERWRITE=yes ;;
	-h | --help) sed -n '2,22p' "$0" >&2; exit 0 ;;
	-*) die "opzione sconosciuta: $1" ;;
	*)
		if [ -z "$ID" ]; then ID=$1; elif [ -z "$VER" ]; then VER=$1; else die "troppi argomenti"; fi ;;
	esac
	shift
done
[ -n "$ID" ] && [ -n "$VER" ] || die "uso: firma-ricetta <recipeId> <recipeVersion> [--verifica] (--help per le opzioni)"
printf '%s\n' "$ID" | grep -Eq '^[a-z0-9][a-z0-9-]{0,63}$' || die "recipeId non valido: $ID"
printf '%s\n' "$VER" | grep -Eq '^[1-9][0-9]*$' || die "recipeVersion non valida: $VER (atteso un intero da 1)"
FILE=$RECIPES_DIR/$ID/$VER.json
SIG=$FILE.sig
[ -r "$FILE" ] || die "ricetta non trovata: $FILE"
command -v openssl >/dev/null 2>&1 || die "serve openssl"
openssl pkeyutl -help 2>&1 | grep -q -- '-rawin' \
	|| die "questo openssl ($(openssl version)) non firma Ed25519 grezzo: serve OpenSSL 3.0 o successivo"

work=$(mktemp -d)
restore_tty() { stty echo 2>/dev/null || true; }
trap 'restore_tty; rm -rf "$work"' EXIT
trap 'exit 130' INT TERM
chmod 0700 "$work"

pub_embedded() {
	printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' "$RECIPES_PUB_B64" >"$work/embedded.pem"
	fp=$(openssl pkey -pubin -in "$work/embedded.pem" -outform DER | sha256sum | cut -d' ' -f1)
	[ "$fp" = "$RECIPES_FINGERPRINT" ] || die "la chiave pubblica scritta in questo script non ha l'impronta dichiarata ($fp): script manomesso?"
}

say "== 1. Valido $FILE"
sh "$here/valida-ricetta.sh" "$FILE" || die "la ricetta non è valida: non si firma (correggerla e ripetere)"

if [ "$VERIFY" = yes ]; then
	say "== 2. Verifico $SIG con la chiave pubblica mt-recipes-2026"
	[ -r "$SIG" ] || die "firma non trovata: $SIG"
	pub_embedded
	# Una riga di base64: 64 byte grezzi. Un .sig malformato si rifiuta prima di openssl.
	tr -d '\r\n' <"$SIG" | grep -Eq '^[A-Za-z0-9+/]{86}==$' || die "$SIG non è una firma Ed25519 in base64 (86 caratteri più ==)"
	tr -d '\r\n' <"$SIG" | openssl base64 -d -A >"$work/sig.raw"
	[ "$(wc -c <"$work/sig.raw")" -eq 64 ] || die "$SIG decodificata non è di 64 byte"
	if openssl pkeyutl -verify -pubin -inkey "$work/embedded.pem" -rawin -in "$FILE" -sigfile "$work/sig.raw" >/dev/null 2>&1; then
		say "   firma valida: $SIG è di mt-recipes-2026 sui byte esatti di $FILE"
		exit 0
	fi
	die "firma NON valida: $SIG non è di mt-recipes-2026 sui byte di $FILE (ricetta cambiata dopo la firma, o altra chiave)"
fi

[ -r "$KEY" ] || die "chiave privata non leggibile: $KEY"
[ -t 0 ] || die "serve un terminale: la passphrase si chiede a video"
if [ -e "$SIG" ] && [ "$OVERWRITE" = no ]; then
	die "$SIG esiste già: una ricetta cambiata è una recipeVersion nuova; per rifirmare la stessa usare --sovrascrivi"
fi

say "== 2. Chiave e passphrase"
# La passphrase si legge una volta, senza eco, e arriva a openssl sullo standard input (printf è un
# comando interno della shell: non compare nell'elenco dei processi). Prima l'eco spento, poi il
# prompt: chi scrive appena lo vede non finisce mai sullo schermo.
stty -echo
printf 'Passphrase di %s: ' "$(basename "$KEY")" >&2
IFS= read -r PASS || PASS=""
restore_tty
printf '\n' >&2
printf '%s\n' "$PASS" | openssl pkey -passin stdin -in "$KEY" -pubout -out "$work/pub.pem" 2>/dev/null \
	|| die "chiave non leggibile: passphrase sbagliata?"
fp=$(openssl pkey -pubin -in "$work/pub.pem" -outform DER | sha256sum | cut -d' ' -f1)
[ "$fp" = "$RECIPES_FINGERPRINT" ] || die "la chiave $KEY non è mt-recipes-2026 (impronta $fp, attesa $RECIPES_FINGERPRINT)"
say "   chiave mt-recipes-2026 (impronta ${fp%"${fp#????????????????}"}…)"

say "== 3. Firmo i byte esatti di $FILE"
printf '%s\n' "$PASS" | openssl pkeyutl -sign -passin stdin -inkey "$KEY" -rawin -in "$FILE" -out "$work/sig.raw"
PASS=""
openssl pkeyutl -verify -pubin -inkey "$work/pub.pem" -rawin -in "$FILE" -sigfile "$work/sig.raw" >/dev/null \
	|| die "la firma appena fatta non si verifica"
[ "$(wc -c <"$work/sig.raw")" -eq 64 ] || die "la firma non è di 64 byte"
openssl base64 -A -in "$work/sig.raw" >"$work/sig.b64"
printf '\n' >>"$work/sig.b64"
cp "$work/sig.b64" "$SIG"
say "   scritto $SIG ($(tr -d '\n' <"$SIG"))"
say "== Fatto. Ora: git add $SIG, commit «feat(recipes): firma di $ID v$VER», PR; poi il super importa la coppia dal repository."
