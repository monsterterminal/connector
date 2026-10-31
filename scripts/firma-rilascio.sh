#!/bin/sh
# Firma di un rilascio del connettore, in un solo comando, sul PC di chi custodisce la chiave (WSL sul
# PC Jack; mai sul server né su GitHub: spec §1.17, §1.19 punto 3). La CI produce una release in BOZZA
# con binari, SHA256SUMS, manifest degli aggiornamenti e install.sh (release.yml); questo script:
#   1. scarica la bozza e controlla gli asset contro SHA256SUMS;
#   2. clona il repository al tag in una cartella temporanea, ricompila con la stessa toolchain della CI
#      (quella di go.mod) e confronta gli hash con SHA256SUMS: è la vera verifica d'integrità, dimostra
#      che la CI non ha alterato nulla;
#   3. controlla che la chiave sia mt-updates-2026 (impronta), chiede la passphrase UNA volta a video,
#      firma SHA256SUMS, ogni manifest e install.sh (Ed25519 grezzo, 64 byte) e verifica subito ogni
#      firma con la chiave pubblica derivata;
#   4. carica le firme e, dopo una conferma, pubblica la release.
#
#   firma-rilascio 0.6.0                         (oppure v0.6.0)
#   firma-rilascio 0.6.0 --key PERCORSO          chiave diversa da ~/chiavi-monsterterminal/mt-updates-2026.key
#   firma-rilascio 0.6.0 --no-publish            carica le firme e lascia la bozza
#   firma-rilascio 0.6.0 --yes                   pubblica senza chiedere conferma
#
# Forma precedente, ancora accettata: --tag vX.Y.Z --key PERCORSO [--publish] [--no-rebuild].
# Serve: gh (autenticato), git, go ≥ 1.21 (scarica da sé la versione di go.mod), openssl ≥ 3.0
# (pkeyutl -rawin), sha256sum. La passphrase non finisce mai in un argomento, in una variabile
# d'ambiente o su disco: passa a openssl sullo standard input.
set -eu

REPO=monsterterminal/connector
TAG=""
KEY=$HOME/chiavi-monsterterminal/mt-updates-2026.key
REBUILD=yes
PUBLISH=yes
CONFIRM=yes
# La chiave deve essere mt-updates-2026 (SECURITY.md): con un'altra, per esempio quella delle ricette,
# le firme sarebbero perfette e nessun host le accetterebbe.
UPDATES_FINGERPRINT=90efb21f2321b1a530efff3973b5d8f4f354675af509958f3f08422cd1c43b0d

die() { printf 'firma-rilascio: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--tag) [ $# -ge 2 ] || die "--tag vuole vX.Y.Z"; TAG=$2; shift ;;
	--key) [ $# -ge 2 ] || die "--key vuole un percorso"; KEY=$2; shift ;;
	--repo) [ $# -ge 2 ] || die "--repo vuole proprietario/nome"; REPO=$2; shift ;;
	--no-rebuild) REBUILD=no ;;
	--publish) PUBLISH=yes ;;
	--no-publish) PUBLISH=no ;;
	--yes) CONFIRM=no ;;
	-h | --help) sed -n '2,23p' "$0" >&2; exit 0 ;;
	-*) die "opzione sconosciuta: $1" ;;
	*) [ -z "$TAG" ] || die "una versione sola"; TAG=$1 ;;
	esac
	shift
done
[ -n "$TAG" ] || die "uso: firma-rilascio X.Y.Z   (--help per le opzioni)"
case "$TAG" in v*) ;; *) TAG=v$TAG ;; esac
VERSION=${TAG#v}
printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || die "versione non valida: $VERSION (attesa X.Y.Z)"
[ -r "$KEY" ] || die "chiave privata non leggibile: $KEY"
for t in gh git openssl sha256sum; do command -v $t >/dev/null 2>&1 || die "serve $t"; done
openssl pkeyutl -help 2>&1 | grep -q -- '-rawin' \
	|| die "questo openssl ($(openssl version)) non firma Ed25519 grezzo: serve OpenSSL 3.0 o successivo"
[ "$REBUILD" = no ] || command -v go >/dev/null 2>&1 || die "serve go per ricompilare e confrontare gli hash"
gh auth status >/dev/null 2>&1 || die "gh non è autenticato: gh auth login"
[ -t 0 ] || die "serve un terminale: la passphrase si chiede a video"

work=$(mktemp -d)
restore_tty() { stty echo 2>/dev/null || true; }
trap 'restore_tty; rm -rf "$work"' EXIT
trap 'exit 130' INT TERM
chmod 0700 "$work"

say "== 1. Scarico la bozza $TAG di $REPO"
gh release view "$TAG" --repo "$REPO" --json isDraft --jq '.isDraft' 2>/dev/null | grep -q true \
	|| die "la release $TAG non esiste o non è una bozza: si firma solo una bozza creata dalla CI"
gh release download "$TAG" --repo "$REPO" --dir "$work/assets"
(cd "$work/assets" && sha256sum -c --quiet SHA256SUMS) || die "gli asset scaricati non corrispondono a SHA256SUMS"
say "   asset coerenti con SHA256SUMS: $(ls "$work/assets" | wc -l) file"

if [ "$REBUILD" = yes ]; then
	say "== 2. Ricompilo $TAG in locale e confronto con gli hash della CI"
	git clone -q --depth 1 --branch "$TAG" "https://github.com/$REPO.git" "$work/src" 2>/dev/null \
		|| die "clone del tag $TAG non riuscito"
	gover=$(sed -n 's/^go \([0-9.]*\)$/\1/p' "$work/src/go.mod")
	[ -n "$gover" ] || die "go.mod senza la riga go X.Y.Z"
	for arch in amd64 arm64; do
		name="monsterterminal-connector_${VERSION}_linux_${arch}"
		theirs=$(grep "  $name\$" "$work/assets/SHA256SUMS" | cut -d' ' -f1)
		[ -n "$theirs" ] || die "$name non è in SHA256SUMS"
		(cd "$work/src" && GOTOOLCHAIN=go$gover CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -buildid= -X main.version=$VERSION" -o "$work/rebuild/$name" ./cmd/monsterterminal-connector)
		mine=$(sha256sum "$work/rebuild/$name" | cut -d' ' -f1)
		[ "$mine" = "$theirs" ] || die "hash DIVERSO per $name: locale $mine, CI $theirs. NON firmare: toolchain diversa o CI compromessa"
		say "   $name: identico (go$gover, $mine)"
	done
else
	say "== 2. Ricompilazione saltata (--no-rebuild): gli hash della CI NON sono verificati"
fi

say "== 3. Chiave e passphrase"
# La passphrase si legge una volta, senza eco, e arriva a openssl sullo standard input (printf è un
# comando interno della shell: non compare nell'elenco dei processi). Con una chiave non cifrata
# openssl la ignora.
# Prima l'eco spento, poi il prompt: chi scrive appena lo vede non finisce mai sullo schermo.
stty -echo
printf 'Passphrase di %s: ' "$(basename "$KEY")" >&2
IFS= read -r PASS || PASS=""
restore_tty
printf '\n' >&2
printf '%s\n' "$PASS" | openssl pkey -passin stdin -in "$KEY" -pubout -out "$work/pub.pem" 2>/dev/null \
	|| die "chiave non leggibile: passphrase sbagliata?"
fp=$(openssl pkey -pubin -in "$work/pub.pem" -outform DER | sha256sum | cut -d' ' -f1)
[ "$fp" = "$UPDATES_FINGERPRINT" ] || die "la chiave $KEY non è mt-updates-2026 (impronta $fp, attesa $UPDATES_FINGERPRINT)"
say "   chiave mt-updates-2026 (impronta ${fp%"${fp#????????????????}"}…)"

sign() {
	printf '%s\n' "$PASS" | openssl pkeyutl -sign -passin stdin -inkey "$KEY" -rawin -in "$1" -out "$1.sig"
	openssl pkeyutl -verify -pubin -inkey "$work/pub.pem" -rawin -in "$1" -sigfile "$1.sig" >/dev/null \
		|| die "la firma appena fatta di $1 non si verifica"
	say "   firmato $(basename "$1")"
}
sign "$work/assets/SHA256SUMS"
sign "$work/assets/install.sh"
for m in "$work/assets"/update-manifest-*.json; do
	[ -e "$m" ] || break
	sign "$m"
done
PASS=""

say "== 4. Carico le firme"
gh release upload "$TAG" --repo "$REPO" --clobber "$work"/assets/*.sig
if [ "$PUBLISH" = no ]; then
	say "== Firme caricate; la release $TAG resta in bozza (--no-publish)"
else
	if [ "$CONFIRM" = yes ]; then
		printf 'Pubblico la release %s di %s? [s/N] ' "$TAG" "$REPO" >&2
		IFS= read -r answer || answer=""
		case "$answer" in s | S | si | sì | Si | Sì) ;; *) say "== Non pubblicata: le firme sono caricate, la release resta in bozza"; exit 0 ;; esac
	fi
	gh release edit "$TAG" --repo "$REPO" --draft=false >/dev/null
	say "== Release $TAG pubblicata: https://github.com/$REPO/releases/tag/$TAG"
fi
say "Chiave pubblica usata (32 byte, base64): $(openssl pkey -in "$work/pub.pem" -pubin -outform DER | tail -c 32 | base64)"
