// DragMeHere dal capo all'altro (schemi 0.7.0, ADR 0409 A1.5): server finto ↔ trasporto ↔ ricevitore su
// una home temporanea. Il file arriva a blocchi da 192 KiB, uno alla volta, in ~/dragmehere/<sottocartella>
// creata al primo blocco, e alla caduta della connessione i trasferimenti aperti si annullano.
package transport_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/files"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/transport"
)

func TestDragMeHereDalCapoAllAltro(t *testing.T) {
	home := t.TempDir()
	s := fakeserver.New()
	t.Cleanup(s.Close)
	id, tok := s.AddHost()
	recv := files.New(files.Config{Home: home})
	r := start(t, s, tok, func(c *transport.Config) {
		c.Files = recv
		c.Sleep = realSleep
		c.Backoff = &transport.Backoff{Min: 20 * time.Millisecond, Max: 100 * time.Millisecond}
	})
	t.Cleanup(r.cancel)
	waitHello(t, s)
	waitWelcome(t, r)
	if got := s.DeclaredSchemas(); len(got) == 0 || got[len(got)-1] != "0.8.0" {
		t.Fatalf("header X-MonsterTerminal-Schema %v, atteso 0.7.0", got)
	}
	reply := func(msgID string) protocol.Envelope {
		t.Helper()
		var env protocol.Envelope
		eventually(t, "risposta a "+msgID, func() bool { var ok bool; env, ok = s.Reply(id, msgID); return ok })
		return env
	}
	send := func(typ string, body any) protocol.Envelope {
		t.Helper()
		msgID, err := s.SendControl(id, typ, body)
		if err != nil {
			t.Fatal(err)
		}
		return reply(msgID)
	}

	data := make([]byte, 450_000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sum := sha256.Sum256(data)
	tid := protocol.NewUUID()
	const chunk = protocol.MaxChunkBytes
	total := (len(data) + chunk - 1) / chunk
	var last protocol.FilePutResult
	for i := 0; i < total; i++ {
		end := min((i+1)*chunk, len(data))
		env := send(protocol.TypeFilePut, protocol.FilePut{TransferID: tid, Path: "consegne", Filename: "schema.pdf",
			Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ChunkIndex: i, TotalChunks: total, Data: data[i*chunk : end]})
		if env.Type != protocol.TypeFilePutResult {
			t.Fatalf("blocco %d: %s %s", i, env.Type, env.Body)
		}
		_ = json.Unmarshal(env.Body, &last)
	}
	if !last.Done || last.Path != "dragmehere/consegne/schema.pdf" || last.Size != int64(len(data)) {
		t.Fatalf("esito finale %+v", last)
	}
	if got, err := os.ReadFile(filepath.Join(home, "dragmehere", "consegne", "schema.pdf")); err != nil || string(got) != string(data) {
		t.Fatalf("file diverso: %v", err)
	}

	// Un trasferimento lasciato a metà sparisce quando cade la connessione.
	tid = protocol.NewUUID()
	env := send(protocol.TypeFilePut, protocol.FilePut{TransferID: tid, Path: "", Filename: "meta.bin", Size: 2 * chunk,
		SHA256: hex.EncodeToString(sum[:]), ChunkIndex: 0, TotalChunks: 2, Data: data[:chunk]})
	if env.Type != protocol.TypeFilePutResult || recv.Alive() != 1 {
		t.Fatalf("primo blocco: %s %s", env.Type, env.Body)
	}
	s.Drop(id)
	eventually(t, "trasferimento annullato alla caduta", func() bool { return recv.Alive() == 0 })
	if _, err := os.Stat(filepath.Join(home, "dragmehere", ".mt-"+tid+".part")); !os.IsNotExist(err) {
		t.Fatal("il temporaneo è rimasto dopo la caduta")
	}
}
