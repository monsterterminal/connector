package protocol

// Test di catalog.install e dei tagli dei testi del catalogo (spec §1.9, schemi 0.11.0).

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeCatalogInstall(t *testing.T) {
	recipe := `{"b":1,"a":"x"}`
	ok := `{"installId":"` + NewUUID() + `","recipe":` + recipe + `,"signature":"qualunque","approvedBy":"` + NewUUID() + `","approvedAt":"2026-11-26T10:00:00.000Z"}`
	c, err := DecodeCatalogInstall([]byte(ok))
	if err != nil {
		t.Fatalf("catalog.install valido rifiutato: %v", err)
	}
	// La ricetta resta nei byte ricevuti, nell'ordine e nella forma in cui è arrivata.
	if string(c.Recipe) != recipe {
		t.Fatalf("ricetta riscritta: %s", c.Recipe)
	}
	bad := map[string]string{
		"installId non uuid":  strings.Replace(ok, `"installId":"`, `"installId":"z`, 1),
		"approvedBy non uuid": strings.Replace(ok, `"approvedBy":"`, `"approvedBy":"z`, 1),
		"approvedAt non UTC":  strings.Replace(ok, `10:00:00.000Z`, `10:00:00+01:00`, 1),
		"ricetta non oggetto": strings.Replace(ok, recipe, `[1]`, 1),
		"campo sconosciuto":   strings.Replace(ok, `"signature"`, `"x":1,"signature"`, 1),
	}
	for name, b := range bad {
		if _, err := DecodeCatalogInstall([]byte(b)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: atteso ErrInvalid, ottenuto %v", name, err)
		}
	}
}

func TestTagliDeiTesti(t *testing.T) {
	if got := TruncateRunes("àèìòù", 3); got != "àèì" {
		t.Fatalf("TruncateRunes: %q", got)
	}
	if got := TruncateRunes("abc", 10); got != "abc" {
		t.Fatalf("TruncateRunes corto: %q", got)
	}
	// La coda di 8 KiB di un testo con caratteri multibyte e byte non validi resta UTF-8 valido e nel tetto.
	s := strings.Repeat("é", 5000) + "\xff\xfe" + "fine"
	got := TailBytes(s, MaxCatalogLogTail)
	if len(got) > MaxCatalogLogTail || !utf8.ValidString(got) || !strings.HasSuffix(got, "?fine") {
		t.Fatalf("TailBytes: %d byte, valido %v, fine %q", len(got), utf8.ValidString(got), got[len(got)-6:])
	}
}
