// Package hostinfo raccoglie i dati dell'host per la registrazione e per hello (§1.3, §1.20).
package hostinfo

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"strings"
)

var (
	machineIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	userRE      = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// Info sono i dati dell'host.
type Info struct {
	MachineID, Hostname, Distro, DistroVersion, Arch, User string
	Root                                                   bool
}

// Arch traduce l'architettura di Go nei valori del protocollo.
func Arch(goarch string) (string, error) {
	switch goarch {
	case "amd64", "arm64":
		return goarch, nil
	}
	return "", fmt.Errorf("architettura %s non supportata: solo amd64 e arm64", goarch)
}

// ParseOSRelease ricava famiglia e versione da /etc/os-release. Le derivate Red Hat (Rocky, Alma,
// CentOS, Fedora) si riportano come rhel, quelle Debian non Ubuntu come debian (§1.20).
func ParseOSRelease(r io.Reader) (distro, version string, err error) {
	vals := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		vals[k] = strings.Trim(v, `"'`)
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	ids := append([]string{vals["ID"]}, strings.Fields(vals["ID_LIKE"])...)
	version = vals["VERSION_ID"]
	if len(version) > 32 {
		version = version[:32]
	}
	for _, id := range ids {
		switch id {
		case "ubuntu":
			return "ubuntu", version, nil
		}
	}
	for _, id := range ids {
		switch id {
		case "debian":
			return "debian", version, nil
		case "rhel", "fedora", "centos":
			return "rhel", version, nil
		}
	}
	return "", "", fmt.Errorf("distribuzione %q non supportata: solo Debian/Ubuntu e Red Hat", vals["ID"])
}

// Collect legge i dati dell'host corrente.
func Collect() (Info, error) {
	var in Info
	raw, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return in, fmt.Errorf("lettura di /etc/machine-id: %w", err)
	}
	in.MachineID = strings.TrimSpace(string(raw))
	if !machineIDRE.MatchString(in.MachineID) {
		return in, errors.New("/etc/machine-id non ha il formato atteso")
	}
	if in.Hostname, err = os.Hostname(); err != nil {
		return in, err
	}
	if len(in.Hostname) > 253 {
		in.Hostname = in.Hostname[:253]
	}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return in, err
	}
	defer f.Close()
	if in.Distro, in.DistroVersion, err = ParseOSRelease(f); err != nil {
		return in, err
	}
	if in.Arch, err = Arch(runtime.GOARCH); err != nil {
		return in, err
	}
	u, err := user.Current()
	if err != nil {
		return in, err
	}
	if !userRE.MatchString(u.Username) {
		return in, fmt.Errorf("nome utente %q non accettato dal protocollo", u.Username)
	}
	in.User, in.Root = u.Username, u.Uid == "0"
	return in, nil
}
