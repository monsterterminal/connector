package protocol

// DragMeHere sugli host del connettore, schemi 0.7.0 (ADR 0409 A1.5): file dal browser verso la home
// dell'utente di servizio, a blocchi in sequenza, e l'elenco delle cartelle per sceglierne una. Nessun
// messaggio esegue nulla: il vocabolario resta chiuso (§1.6).

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

const (
	TypeFilePut          = "file.put"
	TypeFilePutResult    = "file.put.result"
	TypeFileCancel       = "file.cancel"
	TypeFileCancelResult = "file.cancel.result"
	TypeDirList          = "dir.list"
	TypeDirListResult    = "dir.list.result"
)

// Codici di errore di DragMeHere (ADR 0409 A1.5).
const (
	CodePathInvalid      = "PATH_INVALID"
	CodeDirNotFound      = "DIR_NOT_FOUND"
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
	MaxPathBytes      = 1024
	MaxPathSegments   = 16
	MaxDirsListed     = 500
	MaxTransfersAlive = 4
)

var (
	pathSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	fileNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	sha256HexRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsPathSegment applica la regola di un segmento (common.json#pathSegment): niente vuoti, «.», «..» o
// nomi nascosti.
func IsPathSegment(s string) bool { return pathSegmentRE.MatchString(s) }

// IsHomePath applica la regola dei percorsi della home (common.json#homePath): vuoto = la home,
// altrimenti al più 16 segmenti con la regola di un segmento, 1024 byte in tutto. Si controlla per
// segmenti perché l'espressione dello schema supera il tetto delle ripetizioni di RE2.
func IsHomePath(s string) bool {
	if s == "" {
		return true
	}
	segs := strings.Split(s, "/")
	if len(s) > MaxPathBytes || len(segs) > MaxPathSegments {
		return false
	}
	for _, seg := range segs {
		if !IsPathSegment(seg) {
			return false
		}
	}
	return true
}

// IsFileName applica la regola dei nomi di file (common.json#fileName).
func IsFileName(s string) bool { return fileNameRE.MatchString(s) }

// PathSegments divide un percorso valido nei suoi segmenti; vuoto = nessun segmento (la home).
func PathSegments(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

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
	case !IsHomePath(p.Path), !IsFileName(p.Filename):
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

// DirList è il corpo di dir.list.
type DirList struct {
	Path string `json:"path"`
}

// DecodeDirList decodifica dir.list; un percorso fuori regola è ErrPathInvalid.
func DecodeDirList(body []byte) (DirList, error) {
	var m struct {
		Path *string `json:"path"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return DirList{}, err
	}
	if m.Path == nil {
		return DirList{}, fmt.Errorf("dir.list: path mancante: %w", ErrInvalid)
	}
	if !IsHomePath(*m.Path) {
		return DirList{Path: *m.Path}, ErrPathInvalid
	}
	return DirList{Path: *m.Path}, nil
}

// DirListResult è il corpo di dir.list.result.
type DirListResult struct {
	Path      string   `json:"path"`
	Dirs      []string `json:"dirs"`
	Truncated bool     `json:"truncated"`
}
