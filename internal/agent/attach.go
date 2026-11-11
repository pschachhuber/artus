package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif" // registers the formats image.DecodeConfig recognises
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// Limits every provider accepts for a file shown to the model directly.
const (
	maxImage = 5 << 20
	maxPDF   = 20 << 20
)

// attachable reports whether a file can be shown to the model as an image or
// a document, and if not, why. A file a provider rejects would fail every
// later request of the conversation, so anything doubtful is left to tools.
func attachable(path, mediaType string) (ok bool, why string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "it cannot be read"
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif":
		if len(data) > maxImage {
			return false, fmt.Sprintf("images over %d MB are not shown", maxImage>>20)
		}
		if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || "image/"+format != mediaType {
			return false, "it is not a valid " + strings.TrimPrefix(mediaType, "image/") + " image"
		}
	case "image/webp":
		if len(data) > maxImage {
			return false, fmt.Sprintf("images over %d MB are not shown", maxImage>>20)
		}
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return false, "it is not a valid webp image"
		}
	case "application/pdf":
		if len(data) > maxPDF {
			return false, fmt.Sprintf("PDFs over %d MB are not shown", maxPDF>>20)
		}
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			return false, "it is not a valid PDF"
		}
	default:
		return false, mediaType + " files are not shown directly"
	}
	return true, ""
}

// freeze copies a file shown to the model into the chat's uploads under a
// name derived from its content, so the conversation keeps showing what the
// model saw even if the original changes or is deleted later.
func (e *Engine) freeze(chatID, path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return path
	}
	sum := sha256.Sum256(data)
	dst := e.Home.Path("uploads", chatID, "seen-"+hex.EncodeToString(sum[:6])+strings.ToLower(filepath.Ext(path)))
	if _, err := os.Stat(dst); err == nil {
		return dst
	}
	if os.MkdirAll(filepath.Dir(dst), 0o700) != nil || os.WriteFile(dst, data, 0o600) != nil {
		return path
	}
	return dst
}
