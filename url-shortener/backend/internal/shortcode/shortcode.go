// Package shortcode generates random URL-safe short codes.
package shortcode

import (
	"crypto/rand"
	"math/big"
)

// alphabet is the base62 character set used for generated codes.
const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// DefaultLength is the number of characters in a generated code.
const DefaultLength = 7

// Generate returns a cryptographically random base62 code of length n.
func Generate(n int) (string, error) {
	alphabetLen := big.NewInt(int64(len(alphabet)))
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[idx.Int64()]
	}
	return string(b), nil
}
