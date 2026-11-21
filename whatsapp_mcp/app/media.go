package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MediaKind is the WhatsApp message type used for an outgoing file.
type MediaKind string

const (
	KindImage    MediaKind = "image"
	KindVideo    MediaKind = "video"
	KindAudio    MediaKind = "audio"
	KindDocument MediaKind = "document"
)

// OutboxFile is a file read from the outbox.
type OutboxFile struct {
	Name string // base name
	Data []byte
	Mime string
	Kind MediaKind
}

var errOutsideOutbox = errors.New("media_path must be a file inside the outbox folder")

// ReadOutboxFile reads a regular file from the outbox directory. The path may be relative to the
// outbox or absolute under it. os.Root refuses any path (including symlinks) that escapes the outbox,
// so no other file on the system can be sent.
func ReadOutboxFile(outbox, mediaPath string, maxBytes int64) (*OutboxFile, error) {
	rel, err := outboxRelative(outbox, mediaPath)
	if err != nil {
		return nil, err
	}
	if err := RequireRealDir(outbox); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(outbox)
	if err != nil {
		return nil, fmt.Errorf("outbox folder is not available: %w", err)
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("file not found in the outbox folder")
		}
		return nil, errOutsideOutbox
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("media_path is not a regular file")
	}
	if st.Size() > maxBytes {
		return nil, fmt.Errorf("file is larger than the %d MB limit", maxBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file is larger than the %d MB limit", maxBytes>>20)
	}
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	mime, kind := detectMedia(path.Base(rel), data)
	return &OutboxFile{Name: path.Base(rel), Data: data, Mime: mime, Kind: kind}, nil
}

// RequireRealDir refuses a media folder that is (or sits under) a symlink: os.Root confines paths
// below its root, but follows a symlinked root itself, which another add-on with /share access could plant.
func RequireRealDir(dir string) error {
	clean := filepath.Clean(dir)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("media folder is not available: %w", err)
	}
	if real != clean {
		return fmt.Errorf("media folder %s must not be a symlink", clean)
	}
	st, err := os.Lstat(clean)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("media folder %s is not a directory", clean)
	}
	return nil
}

func outboxRelative(outbox, mediaPath string) (string, error) {
	p := strings.TrimSpace(mediaPath)
	if p == "" || strings.ContainsRune(p, 0) {
		return "", errOutsideOutbox
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(filepath.Clean(outbox), filepath.Clean(p))
		if err != nil {
			return "", errOutsideOutbox
		}
		p = rel
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || !fs.ValidPath(p) {
		return "", errOutsideOutbox
	}
	return p, nil
}

// detectMedia picks the WhatsApp message type from the file content (not only the extension).
func detectMedia(name string, data []byte) (string, MediaKind) {
	sniff := http.DetectContentType(data)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch {
	case sniff == "image/jpeg", sniff == "image/png", sniff == "image/webp":
		return sniff, KindImage
	case sniff == "image/gif":
		// WhatsApp has no GIF image type; send as a document.
		return sniff, KindDocument
	case sniff == "video/mp4" || (ext == "mp4" && strings.HasPrefix(sniff, "application/octet-stream")):
		return "video/mp4", KindVideo
	case sniff == "application/ogg" || sniff == "audio/ogg":
		if isOggOpus(data) {
			return "audio/ogg; codecs=opus", KindAudio
		}
		return sniff, KindDocument
	}
	if ext == "pdf" || sniff == "application/pdf" {
		return "application/pdf", KindDocument
	}
	if strings.HasPrefix(sniff, "text/plain") {
		return "text/plain", KindDocument
	}
	return "application/octet-stream", KindDocument
}

func isOggOpus(data []byte) bool {
	if len(data) < 64 || string(data[:4]) != "OggS" {
		return false
	}
	n := 64
	if len(data) < n {
		n = len(data)
	}
	return strings.Contains(string(data[:n]), "OpusHead")
}

// analyzeOggOpus returns the duration (seconds) of an Ogg Opus stream and a waveform for voice notes.
// Bounds-checked rewrite of the parser in lharries/whatsapp-mcp.
func analyzeOggOpus(data []byte) (uint32, []byte, error) {
	if !isOggOpus(data) {
		return 0, nil, errors.New("not an Ogg Opus file")
	}
	var preSkip uint16
	sampleRate := uint32(48000)
	var lastGranule uint64
	for i := 0; i+27 <= len(data); {
		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}
		granule := binary.LittleEndian.Uint64(data[i+6 : i+14])
		nseg := int(data[i+26])
		if i+27+nseg > len(data) {
			break
		}
		size := 27 + nseg
		for _, s := range data[i+27 : i+27+nseg] {
			size += int(s)
		}
		end := i + size
		if end > len(data) {
			end = len(data)
		}
		page := data[i:end]
		if h := strings.Index(string(page), "OpusHead"); h >= 0 && h+16 <= len(page) {
			preSkip = binary.LittleEndian.Uint16(page[h+10 : h+12])
			if sr := binary.LittleEndian.Uint32(page[h+12 : h+16]); sr > 0 {
				sampleRate = sr
			}
		}
		if granule != 0 && granule != math.MaxUint64 {
			lastGranule = granule
		}
		i += size
	}
	secs := uint32(1)
	if lastGranule > uint64(preSkip) {
		secs = uint32(math.Ceil(float64(lastGranule-uint64(preSkip)) / float64(sampleRate)))
	}
	secs = max(1, min(secs, 3600))
	return secs, placeholderWaveform(secs), nil
}

// placeholderWaveform returns a deterministic 64-sample waveform (WhatsApp shows it on voice notes).
func placeholderWaveform(secs uint32) []byte {
	w := make([]byte, 64)
	f := float64(min(secs, 120)) / 30.0
	for i := range w {
		pos := float64(i) / 64
		v := 35*math.Sin(pos*math.Pi*f*8) + 17.5*math.Sin(pos*math.Pi*f*16)
		v = v*(0.7+0.3*math.Sin(pos*math.Pi)) + 50
		w[i] = byte(max(0, min(100, v)))
	}
	return w
}

// safeFileName keeps a received file name usable as a single path element.
func safeFileName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, "\\", "/"))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > 100 {
		out = out[len(out)-100:]
	}
	return out
}
