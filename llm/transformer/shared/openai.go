package shared

import (
	"log/slog"
	"strings"
)

// EncodeOpenAIEncryptedContent encodes raw OpenAI encrypted content for storage.
// OpenAI encrypted_content is already base64-encoded, so this is a passthrough.
func EncodeOpenAIEncryptedContent(content *string) *string {
	if content == nil {
		return nil
	}
	return content
}

// DecodeOpenAIEncryptedContent checks whether a blob is safe to use as OpenAI encrypted content.
// Returns the raw value only if the blob is recognized as OpenAI.
// Returns nil for signatures from other providers (Anthropic/Gemini) or unknown formats.
func DecodeOpenAIEncryptedContent(content *string) *string {
	if content == nil {
		return nil
	}

	result := GuessSignatureProvider(*content)
	if result.Provider != ProviderOpenAI {
		return nil
	}

	if pieces := splitOpenAIEncryptedBlobPieces(*content); len(pieces) > 1 {
		slog.Warn("rejected concatenated OpenAI encrypted content",
			slog.Int("content_len", len(*content)),
			slog.Int("blob_count", len(pieces)),
		)
		return nil
	}

	return content
}

func isOpenAIEncryptedBlob(content string) bool {
	if !strings.HasPrefix(content, "gAAA") {
		return false
	}

	padding := len(content) - len(strings.TrimRight(content, "="))
	if padding > 2 || len(content)%4 != 0 {
		return false
	}

	decodedLen := 3*(len(content)/4) - padding
	if decodedLen < 57+16 {
		return false
	}

	return (decodedLen-57)%16 == 0
}

func splitOpenAIEncryptedBlobPieces(content string) []string {
	var boundaries []int
	for i := 0; i+4 <= len(content); i++ {
		if strings.HasPrefix(content[i:i+4], "gAAA") {
			boundaries = append(boundaries, i)
		}
	}
	if len(boundaries) <= 1 {
		return nil
	}

	pieces := make([]string, 0, len(boundaries))
	for i, start := range boundaries {
		end := len(content)
		if i+1 < len(boundaries) {
			end = boundaries[i+1]
		}
		piece := content[start:end]
		if !isOpenAIEncryptedBlob(piece) {
			return nil
		}
		pieces = append(pieces, piece)
	}

	return pieces
}
