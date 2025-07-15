package helpers

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"unicode"
)

func GenerateSessionToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateNameAcronym creates an acronym from a space name
// Takes the first letter of each word, up to 4 characters
func GenerateNameAcronym(name string) string {
	if name == "" {
		return "?"
	}

	// Split by common word separators
	words := strings.FieldsFunc(name, func(c rune) bool {
		return unicode.IsSpace(c) || c == '-' || c == '_' || c == '.'
	})

	if len(words) == 0 {
		// If no words found, take first character
		runes := []rune(strings.ToUpper(name))
		if len(runes) > 0 {
			return string(runes[0])
		}
		return "?"
	}

	acronym := ""
	for i, word := range words {
		if i >= 4 { // Limit to 4 characters
			break
		}
		if len(word) > 0 {
			// Take first letter of each word
			runes := []rune(strings.ToUpper(word))
			acronym += string(runes[0])
		}
	}

	if acronym == "" {
		return "?"
	}

	return acronym
}

// StringPtr returns a pointer to the given string
func StringPtr(s string) *string {
	return &s
}
