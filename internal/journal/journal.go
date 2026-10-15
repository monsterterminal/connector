// Package journal è il giornale degli eventi sul disco dell'host (spec monsterterminal-saas.md §1.8,
// §1.12, emendamento 2026-10-14).
//
// Ogni evento si scrive qui PRIMA di essere inviato, con un seq crescente e senza buchi. Il backend
// conferma con events.ack fino al suo segnalibro, e il giornale toglie gli eventi confermati. Tiene
// al massimo MaxEvents eventi o MaxAge di età: oltre, scarta i più vecchi e lo DICHIARA con un evento
// journal.truncated nella forma coerente:
//
//	seq          = seq dell'ultimo evento scartato
//	droppedCount = seq − seq dell'elemento che precede gli scartati (il segnalibro, o un troncamento)
//	firstKeptSeq = seq + 1
//
// Un journal.truncated già creato non si modifica mai: se ne è già stato inviato uno, cambiarlo
// renderebbe incoerente quello che il backend ha visto. Nuovi scarti creano un nuovo troncamento.
//
// Fa fede il backend: se il suo segnalibro è indietro rispetto al nostro (ACK_AHEAD, o un events.ack
// più basso), Resync porta il nostro indietro e dichiara persi gli eventi già tolti dal giornale.
//
// Sul disco: journal.jsonl (un evento per riga, append con fsync) e state.json (segnalibro e
// versione dell'ultimo avvio, scritto in modo atomico). Cartella 0700, file 0600.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

// Limiti del giornale (§1.12, decisione BOSS 2026-10-12).
const (
	DefaultMaxEvents = 10_000
	DefaultMaxAge    = 7 * 24 * time.Hour
)

// compactAfter: righe già confermate oltre le quali il file si riscrive con i soli eventi pendenti.
const compactAfter = 500

// Options configura il giornale; i campi a zero prendono i valori di default.
type Options struct {
	MaxEvents int
	MaxAge    time.Duration
	Now       func() time.Time
}

type state struct {
	LastAcked   int64  `json:"lastAcked"`
	LastAckedAt string `json:"lastAckedAt,omitempty"`
	LastVersion string `json:"lastVersion,omitempty"`
}

// Journal è il giornale. È sicuro per l'uso concorrente.
type Journal struct {
	dir       string
	maxEvents int
	maxAge    time.Duration
	now       func() time.Time

	mu         sync.Mutex
	st         state
	pending    []protocol.Event // seq crescenti e contigui da st.LastAcked+1
	nextSeq    int64
	ackedLines int // righe del file con seq <= LastAcked, da compattare
	notify     chan struct{}
}

// ErrDataTooLarge: il data di un evento supera 4 KiB (§1.8).
var ErrDataTooLarge = errors.New("data dell'evento oltre 4 KiB")

// Open apre (o crea) il giornale nella cartella dir.
func Open(dir string, opts Options) (*Journal, error) {
	j := &Journal{
		dir: dir, maxEvents: opts.MaxEvents, maxAge: opts.MaxAge, now: opts.Now,
		notify: make(chan struct{}, 1),
	}
	if j.maxEvents <= 0 {
		j.maxEvents = DefaultMaxEvents
	}
	if j.maxAge <= 0 {
		j.maxAge = DefaultMaxAge
	}
	if j.now == nil {
		j.now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if err := j.load(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Journal) eventsPath() string { return filepath.Join(j.dir, "journal.jsonl") }
func (j *Journal) statePath() string  { return filepath.Join(j.dir, "state.json") }

func (j *Journal) load() error {
	if raw, err := os.ReadFile(j.statePath()); err == nil {
		if err := json.Unmarshal(raw, &j.st); err != nil {
			return fmt.Errorf("giornale: state.json illeggibile: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.Open(j.eventsPath())
	if errors.Is(err, os.ErrNotExist) {
		j.nextSeq = j.st.LastAcked + 1
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	seen := map[int64]protocol.Event{}
	maxSeq := j.st.LastAcked
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		var ev protocol.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			// Una riga troncata da uno spegnimento brusco a metà scrittura: l'ultima. Le altre no.
			continue
		}
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
		if ev.Seq <= j.st.LastAcked {
			j.ackedLines++
			continue
		}
		seen[ev.Seq] = ev
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, ev := range seen {
		j.pending = append(j.pending, ev)
	}
	sort.Slice(j.pending, func(a, b int) bool { return j.pending[a].Seq < j.pending[b].Seq })
	// Invariante: pendenti contigui da LastAcked+1. Un buco (file manomesso o perso) si dichiara
	// come troncamento invece di mandare un SEQ_GAP al backend.
	j.pending = j.closeGaps(j.pending)
	j.nextSeq = maxSeq + 1
	if err := j.enforceLimits(); err != nil {
		return err
	}
	return j.rewrite()
}

// closeGaps tiene la sequenza contigua: se manca un intervallo, lo copre con un troncamento.
func (j *Journal) closeGaps(evs []protocol.Event) []protocol.Event {
	out := evs[:0:0]
	prev := j.st.LastAcked
	for _, ev := range evs {
		// Un troncamento copre da sé il salto che lo precede.
		if ev.Seq > prev+1 && ev.Kind != protocol.KindJournalTruncated {
			at := protocol.Timestamp(j.now())
			out = append(out, truncation(ev.Seq-1, ev.Seq-1-prev, at, at, at))
		}
		out = append(out, ev)
		prev = ev.Seq
	}
	return out
}

func truncation(seq, dropped int64, oldest, newest, at string) protocol.Event {
	data, _ := json.Marshal(protocol.JournalTruncated{
		DroppedCount: dropped, FirstKeptSeq: seq + 1, OldestDroppedAt: oldest, NewestDroppedAt: newest,
	})
	return protocol.Event{Seq: seq, Kind: protocol.KindJournalTruncated, At: at, Data: data}
}

// Append scrive un evento nuovo e lo restituisce col suo seq.
func (j *Journal) Append(kind string, data any) (protocol.Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return protocol.Event{}, err
	}
	if string(raw) == "null" {
		raw = []byte("{}") // data è sempre un oggetto (schema event.json)
	}
	if len(raw) > protocol.MaxEventDataBytes {
		return protocol.Event{}, ErrDataTooLarge
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.nextSeq > protocol.MaxCounter {
		return protocol.Event{}, errors.New("giornale: seq oltre il tetto 2^53-1")
	}
	ev := protocol.Event{Seq: j.nextSeq, Kind: kind, At: protocol.Timestamp(j.now()), Data: raw}
	if err := j.appendLines(ev); err != nil {
		return protocol.Event{}, err
	}
	j.nextSeq++
	j.pending = append(j.pending, ev)
	n := len(j.pending)
	if err := j.enforceLimits(); err != nil {
		return ev, err
	}
	if len(j.pending) != n {
		if err := j.rewrite(); err != nil {
			return ev, err
		}
	}
	j.signal()
	return ev, nil
}

func (j *Journal) appendLines(evs ...protocol.Event) error {
	f, err := os.OpenFile(j.eventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		line, err := json.Marshal(ev)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// enforceLimits scarta i più vecchi oltre MaxEvents o MaxAge, a blocchi del 10%, e dichiara lo
// scarto con un journal.truncated. Da chiamare col lock.
func (j *Journal) enforceLimits() error {
	cutoff := j.now().Add(-j.maxAge)
	for {
		// Primo evento scartabile: si saltano i troncamenti in testa, che non si scartano mai.
		start := 0
		for start < len(j.pending) && j.pending[start].Kind == protocol.KindJournalTruncated {
			start++
		}
		if start >= len(j.pending) {
			return nil
		}
		drop := 0
		if len(j.pending) > j.maxEvents {
			chunk := j.maxEvents / 10
			if chunk < 1 {
				chunk = 1
			}
			drop = len(j.pending) - j.maxEvents + chunk
		}
		// Gli scaduti per età, sempre in testa (gli eventi sono in ordine di tempo).
		aged := 0
		for i := start; i < len(j.pending); i++ {
			at, err := time.Parse(time.RFC3339Nano, j.pending[i].At)
			if err != nil || !at.Before(cutoff) {
				break
			}
			aged++
		}
		if aged > drop {
			drop = aged
		}
		if drop == 0 {
			return nil
		}
		if start+drop > len(j.pending) {
			drop = len(j.pending) - start
		}
		first, last := j.pending[start], j.pending[start+drop-1]
		before := j.st.LastAcked
		if start > 0 {
			before = j.pending[start-1].Seq
		}
		t := truncation(last.Seq, last.Seq-before, first.At, last.At, protocol.Timestamp(j.now()))
		rest := append([]protocol.Event{}, j.pending[:start]...)
		rest = append(rest, t)
		rest = append(rest, j.pending[start+drop:]...)
		j.pending = rest
	}
}

// LastAcked è il segnalibro: tutto fino a qui è confermato dal backend.
func (j *Journal) LastAcked() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.st.LastAcked
}

// Pending restituisce al più max eventi con seq > after, in ordine.
func (j *Journal) Pending(after int64, max int) []protocol.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []protocol.Event
	for _, ev := range j.pending {
		if ev.Seq <= after {
			continue
		}
		if len(out) >= max {
			break
		}
		out = append(out, ev)
	}
	return out
}

// Len è il numero di eventi pendenti.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.pending)
}

// Ack registra la conferma del backend fino a upTo (§1.12) e toglie gli eventi confermati. Un upTo
// più basso del segnalibro non lo abbassa: per quello c'è Resync.
func (j *Journal) Ack(upTo int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if upTo <= j.st.LastAcked {
		return nil
	}
	if upTo >= j.nextSeq {
		upTo = j.nextSeq - 1 // il backend non può confermare ciò che non è stato scritto
	}
	removed := 0
	at := j.st.LastAckedAt
	for removed < len(j.pending) && j.pending[removed].Seq <= upTo {
		at = j.pending[removed].At
		removed++
	}
	j.pending = j.pending[removed:]
	j.st.LastAcked = upTo
	j.st.LastAckedAt = at
	j.ackedLines += removed
	if err := j.saveState(); err != nil {
		return err
	}
	if j.ackedLines > compactAfter {
		return j.rewrite()
	}
	return nil
}

// Resync porta il segnalibro al valore del backend, che fa fede (ACK_AHEAD o un events.ack più
// basso del nostro, §1.12). Gli eventi bookmark+1..LastAcked sono già stati tolti dal giornale: si
// dichiarano persi con un journal.truncated che prende il seq LastAcked. Gli istanti sono quelli
// dell'ultima conferma ricevuta, l'unico che il giornale conosce di quegli eventi.
func (j *Journal) Resync(bookmark int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if bookmark < 0 || bookmark >= j.st.LastAcked {
		return nil
	}
	L := j.st.LastAcked
	at := j.st.LastAckedAt
	if at == "" {
		at = protocol.Timestamp(j.now())
	}
	t := truncation(L, L-bookmark, at, at, protocol.Timestamp(j.now()))
	j.pending = append([]protocol.Event{t}, j.pending...)
	j.st.LastAcked = bookmark
	// Riscrittura completa, non append: il file può contenere ancora righe confermate in
	// bookmark+1..L non compattate, che al riavvio tornerebbero pendenti accanto al troncamento.
	if err := j.rewrite(); err != nil {
		return err
	}
	j.signal()
	return nil
}

// LastVersion e SetLastVersion conservano la versione dell'ultimo avvio (per connector.restarted).
func (j *Journal) LastVersion() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.st.LastVersion
}

func (j *Journal) SetLastVersion(v string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.st.LastVersion = v
	return j.saveState()
}

// Notify si sveglia quando arriva un evento nuovo o un Resync.
func (j *Journal) Notify() <-chan struct{} { return j.notify }

func (j *Journal) signal() {
	select {
	case j.notify <- struct{}{}:
	default:
	}
}

func (j *Journal) saveState() error {
	raw, err := json.Marshal(j.st)
	if err != nil {
		return err
	}
	return writeAtomic(j.statePath(), raw)
}

// rewrite riscrive il file con i soli eventi pendenti. Da chiamare col lock.
func (j *Journal) rewrite() error {
	var buf []byte
	for _, ev := range j.pending {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := writeAtomic(j.eventsPath(), buf); err != nil {
		return err
	}
	j.ackedLines = 0
	return j.saveState()
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Rebase rende coerente un journal.truncated col segnalibro che il backend avrà quando lo riceve:
// prev è il seq dell'evento inviato subito prima nella stessa connessione (o il segnalibro appena
// sincronizzato). Serve perché il backend può aver già accettato una parte degli eventi scartati
// mentre erano in volo: il troncamento dichiara allora solo prev+1..seq. Gli altri eventi passano
// invariati. Se il backend ha già il troncamento, lo tratta da duplicato e i campi non contano.
func Rebase(ev protocol.Event, prev int64) protocol.Event {
	if ev.Kind != protocol.KindJournalTruncated || prev >= ev.Seq {
		return ev
	}
	var d protocol.JournalTruncated
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return ev
	}
	d.DroppedCount = ev.Seq - prev
	d.FirstKeptSeq = ev.Seq + 1
	raw, err := json.Marshal(d)
	if err != nil {
		return ev
	}
	ev.Data = raw
	return ev
}
