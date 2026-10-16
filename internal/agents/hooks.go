package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// HookEvents sono gli eventi di Claude Code che l'adattatore ascolta (spec §1.21).
var HookEvents = []string{"UserPromptSubmit", "Notification", "Stop", "StopFailure", "SessionEnd"}

// hookTimeoutSeconds: il comando `hook` esce subito; il tetto protegge Claude Code se il socket non risponde.
const hookTimeoutSeconds = 10

// FindClaude cerca Claude Code sull'host: nel PATH, poi nei percorsi d'installazione più comuni.
func FindClaude(home string) (string, bool) {
	if p, err := exec.LookPath("claude"); err == nil {
		return p, true
	}
	for _, p := range []string{
		filepath.Join(home, ".npm-global", "bin", "claude"),
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/usr/local/bin/claude",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// SettingsPath è il file delle impostazioni utente di Claude Code.
func SettingsPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }

// isOurs riconosce un comando di hook scritto da questo connettore, anche se il binario è stato spostato.
func isOurs(cmd string) bool {
	return strings.Contains(cmd, "monsterterminal-connector") && strings.Contains(cmd, " hook ")
}

// HookCommand è il comando che Claude Code esegue per un evento.
func HookCommand(bin, stateDir, event string) (string, error) {
	for _, s := range []string{bin, stateDir} {
		if s == "" || strings.ContainsAny(s, " \t\n'\"\\$`;&|<>") {
			return "", fmt.Errorf("percorso non utilizzabile in un comando di hook: %q", s)
		}
	}
	return fmt.Sprintf("%s hook -state-dir %s %s", bin, stateDir, event), nil
}

func readSettings(path string) (map[string]any, os.FileMode, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, 0o600, nil
	}
	if err != nil {
		return nil, 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	var m map[string]any
	if len(strings.TrimSpace(string(raw))) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(raw, &m); err != nil {
		return nil, 0, fmt.Errorf("%s non è JSON valido: non lo modifico: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, fi.Mode().Perm(), nil
}

func writeSettings(path string, m map[string]any, mode os.FileMode, now time.Time) (backup string, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if old, err := os.ReadFile(path); err == nil {
		backup = fmt.Sprintf("%s.monsterterminal-%s.bak", path, now.UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(backup, old, mode); err != nil {
			return "", err
		}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return backup, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return backup, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return backup, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return backup, err
	}
	if err := tmp.Close(); err != nil {
		return backup, err
	}
	return backup, os.Rename(name, path)
}

func hooksMap(m map[string]any) (map[string]any, error) {
	h, ok := m["hooks"]
	if !ok {
		return map[string]any{}, nil
	}
	hm, ok := h.(map[string]any)
	if !ok {
		return nil, errors.New(`"hooks" nelle impostazioni non è un oggetto: non lo modifico`)
	}
	return hm, nil
}

func entryHasOurs(entry any) bool {
	e, _ := entry.(map[string]any)
	list, _ := e["hooks"].([]any)
	for _, h := range list {
		hm, _ := h.(map[string]any)
		if c, _ := hm["command"].(string); isOurs(c) {
			return true
		}
	}
	return false
}

// Installed dice se gli hook del connettore sono nelle impostazioni (capacità `hooks` di hello).
func Installed(path string) bool {
	m, _, err := readSettings(path)
	if err != nil {
		return false
	}
	hm, err := hooksMap(m)
	if err != nil {
		return false
	}
	for _, ev := range HookEvents {
		list, _ := hm[ev].([]any)
		found := false
		for _, e := range list {
			if entryHasOurs(e) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Install aggiunge gli hook mancanti e lascia intatti tutti gli altri. Idempotente: se sono già tutti
// presenti non scrive nulla. Prima di scrivere copia il file com'era.
func Install(path, bin, stateDir string, now time.Time) (changed bool, backup string, err error) {
	m, mode, err := readSettings(path)
	if err != nil {
		return false, "", err
	}
	hm, err := hooksMap(m)
	if err != nil {
		return false, "", err
	}
	for _, ev := range HookEvents {
		cmd, err := HookCommand(bin, stateDir, ev)
		if err != nil {
			return false, "", err
		}
		list, _ := hm[ev].([]any)
		present := false
		for _, e := range list {
			if entryHasOurs(e) {
				present = true
			}
		}
		if present {
			continue
		}
		hm[ev] = append(list, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": hookTimeoutSeconds}},
		})
		changed = true
	}
	if !changed {
		return false, "", nil
	}
	m["hooks"] = hm
	backup, err = writeSettings(path, m, mode, now)
	return true, backup, err
}

// Remove toglie solo gli hook del connettore. Idempotente.
func Remove(path string, now time.Time) (changed bool, backup string, err error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	m, mode, err := readSettings(path)
	if err != nil {
		return false, "", err
	}
	hm, err := hooksMap(m)
	if err != nil {
		return false, "", err
	}
	for ev, v := range hm {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		var keep []any
		for _, e := range list {
			em, ok := e.(map[string]any)
			if !ok {
				keep = append(keep, e)
				continue
			}
			hooks, _ := em["hooks"].([]any)
			var rest []any
			for _, h := range hooks {
				hmm, _ := h.(map[string]any)
				if c, _ := hmm["command"].(string); isOurs(c) {
					changed = true
					continue
				}
				rest = append(rest, h)
			}
			if len(rest) == 0 && len(hooks) > 0 {
				continue // la voce conteneva solo i nostri
			}
			em["hooks"] = rest
			keep = append(keep, em)
		}
		if len(keep) == 0 {
			delete(hm, ev)
		} else {
			hm[ev] = keep
		}
	}
	if !changed {
		return false, "", nil
	}
	if len(hm) == 0 {
		delete(m, "hooks")
	} else {
		m["hooks"] = hm
	}
	backup, err = writeSettings(path, m, mode, now)
	return true, backup, err
}
