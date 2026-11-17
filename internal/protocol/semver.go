package protocol

import (
	"strconv"
	"strings"
)

// CompareSemver confronta due versioni semver 2.0 (senza i metadati di build, che si ignorano):
// -1, 0, 1. Una release precede le sue pre-release; i campi numerici delle pre-release si confrontano
// per valore, quelli alfanumerici per ordine ASCII, e un identificatore numerico precede uno
// alfanumerico. Lo usano connector.restarted (motivo update/rollback) e l'aggiornamento automatico
// (ADR 0438 D3: si installa solo una versione strettamente maggiore).
func CompareSemver(a, b string) int {
	a, _, _ = strings.Cut(a, "+")
	b, _, _ = strings.Cut(b, "+")
	ac, apre, _ := strings.Cut(a, "-")
	bc, bpre, _ := strings.Cut(b, "-")
	ap, bp := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < 3 && i < len(ap) && i < len(bp); i++ {
		x, _ := strconv.Atoi(ap[i])
		y, _ := strconv.Atoi(bp[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "": // una release precede le sue pre-release
		return 1
	case bpre == "":
		return -1
	}
	ai, bi := strings.Split(apre, "."), strings.Split(bpre, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		if ai[i] == bi[i] {
			continue
		}
		x, xerr := strconv.Atoi(ai[i])
		y, yerr := strconv.Atoi(bi[i])
		switch {
		case xerr == nil && yerr == nil:
			if x < y {
				return -1
			}
			return 1
		case xerr == nil: // numerico < alfanumerico
			return -1
		case yerr == nil:
			return 1
		case ai[i] < bi[i]:
			return -1
		default:
			return 1
		}
	}
	switch {
	case len(ai) < len(bi):
		return -1
	case len(ai) > len(bi):
		return 1
	}
	return 0
}
