#!/bin/sh
# Firma di un rilascio del connettore, da lanciare sul PC di chi custodisce la chiave (mai sul server:
# spec §1.17, §1.19 punto 3). La CI produce una release in BOZZA con i binari, SHA256SUMS, i manifest
# degli aggiornamenti e install.sh (release.yml); questo script:
#   1. scarica la bozza;
#   2. ricompila in locale con la stessa toolchain e confronta gli hash con SHA256SUMS (è la vera
#      verifica d'integrità: dimostra che la CI non ha alterato nulla; --no-rebuild la salta);
#   3. firma SHA256SUMS, ogni manifest e install.sh con la chiave Ed25519 (openssl pkeyutl, firma
#      grezza di 64 byte), verifica subito ogni firma con la chiave pubblica derivata;
#   4. carica le firme nella release; con --publish la toglie dalla bozza.
#
#   sh scripts/firma-rilascio.sh --tag v0.2.0 --key ~/chiavi/mt-updates-2026.key [--no-rebuild] [--publish]
#
# Serve: gh (autenticato), go (la versione la decide go.mod), openssl ≥ 1.1.1, sha256sum.
set -eu

REPO=monsterterminal/connector
TAG=""
KEY=""
REBUILD=yes
PUBLISH=no

die() { printf 'firma-rilascio: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--tag) TAG=$2; shift ;;
	--key) KEY=$2; shift ;;
	--no-rebuild) REBUILD=no ;;
	--publish) PUBLISH=yes ;;
	--repo) REPO=$2; shift ;;
	*) die "opzione sconosciuta: $1" ;;
	esac
	shift
done
[ -n "$TAG" ] || die "--tag vX.Y.Z obbligatorio"
[ -r "$KEY" ] || die "--key: file della chiave privata non leggibile"
case "$TAG" in v*) ;; *) die "il tag inizia con v" ;; esac
VERSION=${TAG#v}
for t in gh openssl sha256sum; do command -v $t >/dev/null 2>&1 || die "serve $t"; done
[ "$REBUILD" = no ] || command -v go >/dev/null 2>&1 || die "serve go per ricompilare (o --no-rebuild, sconsigliato)"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
chmod 0700 "$work"

printf '== Scarico la bozza %s di %s\n' "$TAG" "$REPO"
gh release view "$TAG" --repo "$REPO" --json isDraft --jq '.isDraft' | grep -q true \
	|| die "la release $TAG non è una bozza: la CI deve crearla come draft, non si firma una release già pubblica"
gh release download "$TAG" --repo "$REPO" --dir "$work/assets"
ls "$work/assets" >&2
(cd "$work/assets" && sha256sum -c --quiet SHA256SUMS) || die "gli asset scaricati non corrispondono a SHA256SUMS"

if [ "$REBUILD" = yes ]; then
	printf '== Ricompilo in locale e confronto con SHA256SUMS\n'
	root=$(cd "$(dirname "$0")/.." && pwd)
	[ "$(git -C "$root" describe --tags --exact-match 2>/dev/null)" = "$TAG" ] \
		|| die "il checkout in $root non è al tag $TAG: git checkout $TAG prima di firmare"
	for arch in amd64 arm64; do
		name="monsterterminal-connector_${VERSION}_linux_${arch}"
		(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -buildid= -X main.version=$VERSION" -o "$work/rebuild/$name" ./cmd/monsterterminal-connector)
		mine=$(sha256sum "$work/rebuild/$name" | cut -d' ' -f1)
		theirs=$(grep "  $name\$" "$work/assets/SHA256SUMS" | cut -d' ' -f1)
		[ "$mine" = "$theirs" ] || die "hash DIVERSO per $name: locale $mine, CI $theirs. NON firmare: toolchain diversa o CI compromessa"
		printf '   %s: identico (%s)\n' "$name" "$mine"
	done
fi

printf '== Firmo con %s\n' "$KEY"
openssl pkey -in "$KEY" -pubout -out "$work/pub.pem"
# La chiave deve essere mt-updates-2026 (SECURITY.md): con un'altra, per esempio quella delle
# ricette, le firme sarebbero perfette e nessun host le accetterebbe.
UPDATES_FINGERPRINT=90efb21f2321b1a530efff3973b5d8f4f354675af509958f3f08422cd1c43b0d
fp=$(openssl pkey -pubin -in "$work/pub.pem" -outform DER | sha256sum | cut -d' ' -f1)
[ "$fp" = "$UPDATES_FINGERPRINT" ] || die "la chiave $KEY non è mt-updates-2026 (impronta $fp, attesa $UPDATES_FINGERPRINT)"
sign() {
	openssl pkeyutl -sign -inkey "$KEY" -rawin -in "$1" -out "$1.sig"
	openssl pkeyutl -verify -pubin -inkey "$work/pub.pem" -rawin -in "$1" -sigfile "$1.sig" >/dev/null \
		|| die "la firma appena fatta di $1 non si verifica: chiave sbagliata?"
	printf '   firmato %s\n' "$(basename "$1")"
}
sign "$work/assets/SHA256SUMS"
sign "$work/assets/install.sh"
for m in "$work/assets"/update-manifest-*.json; do
	[ -e "$m" ] || break
	sign "$m"
done

printf '== Carico le firme\n'
gh release upload "$TAG" --repo "$REPO" --clobber "$work"/assets/*.sig
if [ "$PUBLISH" = yes ]; then
	gh release edit "$TAG" --repo "$REPO" --draft=false
	printf '== Release %s pubblicata\n' "$TAG"
else
	printf '== Firme caricate; la release resta in bozza (--publish per pubblicarla)\n'
fi
printf 'Chiave pubblica usata (32 byte, base64): %s\n' "$(openssl pkey -in "$work/pub.pem" -pubin -outform DER | tail -c 32 | base64)"
