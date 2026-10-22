package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/provider"
)

// maxInlineTextSize bounds the text one tool result or resource block adds
// to the conversation. Longer text is saved to a temporary file and only
// its beginning is returned, with the path, so a large page or dump
// cannot fill the context window.
const maxInlineTextSize = 64 << 10

// tempFile creates a private file for content that is handed to the model
// by path. Files are tracked so Close removes them with the runtime.
func (m *Manager) tempFile(pattern string, data []byte) (string, error) {
	file, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	m.mu.Lock()
	m.tempFiles = append(m.tempFiles, file.Name())
	m.mu.Unlock()
	return file.Name(), nil
}

// removeTempFiles deletes the files handed out during the runtime.
func (m *Manager) removeTempFiles() {
	m.mu.Lock()
	files := m.tempFiles
	m.tempFiles = nil
	m.mu.Unlock()
	for _, path := range files {
		os.Remove(path)
	}
}

// boundedText returns text unchanged when it fits, and otherwise its
// beginning plus a note with the file holding the complete text. origin
// names the source for the note, for example a tool or resource.
func (m *Manager) boundedText(origin, text string) provider.TextBlock {
	if len(text) <= maxInlineTextSize {
		return provider.TextBlock{Text: text}
	}
	path, err := m.tempFile("zot-mcp-*.txt", []byte(text))
	if err != nil {
		return provider.TextBlock{Text: fmt.Sprintf("%s\n... [truncated at %d bytes, %d bytes total, the complete text could not be saved]", headOf(text), maxInlineTextSize, len(text))}
	}
	return provider.TextBlock{Text: fmt.Sprintf("%s\n... [truncated at %d bytes, %d bytes total from %s, complete text saved to %s]", headOf(text), maxInlineTextSize, len(text), origin, filepath.ToSlash(path))}
}

// headOf cuts text at the inline limit without splitting a character.
func headOf(text string) string {
	cut := maxInlineTextSize
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], "\r\n")
}
