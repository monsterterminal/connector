package protocol

// DragMeHere sugli host del connettore, schemi 0.7.0 (ADR 0409 A1.5): file dal browser verso
// ~/dragmehere (o una sua sottocartella di un livello) nella home dell'utente di servizio, a blocchi in
// sequenza. Niente navigazione delle cartelle (decisione BOSS 2026-10-27). Nessun messaggio esegue
// nulla: il vocabolario resta chiuso (§1.6).

import (
	"encoding/base64"
	"fmt"
	"regexp"
)

const (
	TypeFilePut          = "file.put"
	TypeFilePutResult    = "file.put.result"
	TypeFileCancel       = "file.cancel"
	TypeFileCancelResult = "file.cancel.result"
)

// Codici di errore di DragMeHere (ADR 0409 A1.5).
const (
	CodePathInvalid      = "PATH_INVALID"
	CodeFileExists       = "FILE_EXISTS"
	CodeFileTooLarge     = "FILE_TOO_LARGE"
	CodeDiskFull         = "DISK_FULL"
	CodeTransferLimit    = "TRANSFER_LIMIT"
	CodeTransferUnknown  = "TRANSFER_UNKNOWN"
	CodeChecksumMismatch = "CHECKSUM_MISMATCH"
	CodeProtocolError    = "PROTOCOL_ERROR"
)

// Tetti di DragMeHere (schemi 0.7.0).
const (
	MaxFileBytes      = 50 << 20 // il tetto di DragMeHere
	MaxChunkBase64    = 262144   // caratteri di data, cioè 192 KiB decodificati
	MaxChunkBytes     = 192 << 10
	MaxTransfersAlive = 4
	// DropDir è la cartella di DragMeHere nella home dell'utente di servizio.
	DropDir = "dragmehere"
)

var (
	pathSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	fileNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	sha256HexRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsPathSegment applica la regola di un segmento (common.json#pathSegment): niente vuoti, «.», «..» o
// nomi nascosti.
func IsPathSegment(s string) bool { return pathSegmentRE.MatchString(s) }

// IsDropSubdir applica la regola della sottocartella di ~/dragmehere (common.json#dragmehereDir): vuoto
// = ~/dragmehere stessa, altrimenti un solo segmento.
func IsDropSubdir(s string) bool { return s == "" || IsPathSegment(s) }

// IsFileName applica la regola dei nomi di file (common.json#fileName).
func IsFileName(s string) bool { return fileNameRE.MatchString(s) }

// FilePut è un blocco di file.put, con i dati già decodificati.
type FilePut struct {
	TransferID  string `json:"transferId"`
	Path        string `json:"path"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Overwrite   bool   `json:"overwrite,omitempty"`
	ChunkIndex  int    `json:"chunkIndex"`
	TotalChunks int    `json:"totalChunks"`
	Data        []byte `json:"data"` // base64 nel JSON, come lo codifica encoding/json
}

// ErrPathInvalid distingue un percorso o un nome fuori regola (risposta PATH_INVALID).
var ErrPathInvalid = fmt.Errorf("percorso fuori regola: %w", ErrInvalid)

// DecodeFilePut decodifica e valida un blocco di file.put. Un percorso o un nome fuori regola è
// ErrPathInvalid; una dimensione oltre il tetto è ErrFileTooLarge.
func DecodeFilePut(body []byte) (FilePut, error) {
	var m struct {
		TransferID  *string `json:"transferId"`
		Path        *string `json:"path"`
		Filename    *string `json:"filename"`
		Size        *int64  `json:"size"`
		SHA256      *string `json:"sha256"`
		Overwrite   *bool   `json:"overwrite"`
		ChunkIndex  *int    `json:"chunkIndex"`
		TotalChunks *int    `json:"totalChunks"`
		Data        *string `json:"data"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return FilePut{}, err
	}
	if m.TransferID == nil || m.Path == nil || m.Filename == nil || m.Size == nil || m.SHA256 == nil ||
		m.ChunkIndex == nil || m.TotalChunks == nil || m.Data == nil {
		return FilePut{}, fmt.Errorf("file.put: campi obbligatori mancanti: %w", ErrInvalid)
	}
	p := FilePut{TransferID: *m.TransferID, Path: *m.Path, Filename: *m.Filename, Size: *m.Size, SHA256: *m.SHA256,
		ChunkIndex: *m.ChunkIndex, TotalChunks: *m.TotalChunks}
	if m.Overwrite != nil {
		p.Overwrite = *m.Overwrite
	}
	switch {
	case !IsUUID(p.TransferID):
		return p, fmt.Errorf("file.put: transferId non è un UUID: %w", ErrInvalid)
	case !sha256HexRE.MatchString(p.SHA256):
		return p, fmt.Errorf("file.put: sha256 non è esadecimale di 64 caratteri: %w", ErrInvalid)
	case p.ChunkIndex < 0 || p.TotalChunks < 1:
		return p, fmt.Errorf("file.put: chunkIndex %d, totalChunks %d: %w", p.ChunkIndex, p.TotalChunks, ErrInvalid)
	case p.Size < 1:
		return p, fmt.Errorf("file.put: size %d: %w", p.Size, ErrInvalid)
	case p.Size > MaxFileBytes:
		return p, ErrFileTooLarge
	case len(*m.Data) > MaxChunkBase64:
		return p, fmt.Errorf("file.put: data oltre %d caratteri: %w", MaxChunkBase64, ErrInvalid)
	case !IsDropSubdir(p.Path), !IsFileName(p.Filename):
		return p, ErrPathInvalid
	}
	data, err := base64.StdEncoding.Strict().DecodeString(*m.Data)
	if err != nil {
		return p, fmt.Errorf("file.put: data non è base64: %w", ErrInvalid)
	}
	p.Data = data
	return p, nil
}

// ErrFileTooLarge: la dimensione dichiarata supera il tetto (risposta FILE_TOO_LARGE).
var ErrFileTooLarge = fmt.Errorf("file oltre %d MiB: %w", MaxFileBytes>>20, ErrInvalid)

// FilePutResult è l'esito di un blocco; Path, Size e SHA256 solo con Done.
type FilePutResult struct {
	TransferID string `json:"transferId"`
	ChunkIndex int    `json:"chunkIndex"`
	Received   int64  `json:"received"`
	Done       bool   `json:"done"`
	Path       string `json:"path,omitempty"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

// TransferBody è il corpo di file.cancel e file.cancel.result.
type TransferBody struct {
	TransferID string `json:"transferId"`
}

// DecodeFileCancel decodifica file.cancel.
func DecodeFileCancel(body []byte) (TransferBody, error) {
	var m struct {
		TransferID *string `json:"transferId"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return TransferBody{}, err
	}
	if m.TransferID == nil || !IsUUID(*m.TransferID) {
		return TransferBody{}, fmt.Errorf("file.cancel: transferId mancante o non UUID: %w", ErrInvalid)
	}
	return TransferBody{TransferID: *m.TransferID}, nil
}
