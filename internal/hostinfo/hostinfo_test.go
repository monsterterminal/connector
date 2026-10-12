package hostinfo

import (
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	cases := []struct{ in, distro, version string }{
		{"ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n", "ubuntu", "24.04"},
		{"ID=debian\nVERSION_ID=\"12\"\n", "debian", "12"},
		{"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"9.4\"\n", "rhel", "9.4"},
		{"ID=\"rhel\"\nVERSION_ID=\"9.4\"\n", "rhel", "9.4"},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\nVERSION_ID=\"22\"\n", "ubuntu", "22"},
	}
	for _, c := range cases {
		d, v, err := ParseOSRelease(strings.NewReader(c.in))
		if err != nil || d != c.distro || v != c.version {
			t.Errorf("%q: ottenuto %s %s %v", c.in, d, v, err)
		}
	}
	if _, _, err := ParseOSRelease(strings.NewReader("ID=alpine\n")); err == nil {
		t.Error("alpine accettata")
	}
}

func TestArch(t *testing.T) {
	for _, a := range []string{"amd64", "arm64"} {
		if got, err := Arch(a); err != nil || got != a {
			t.Errorf("%s rifiutata", a)
		}
	}
	if _, err := Arch("386"); err == nil {
		t.Error("386 accettata")
	}
}
