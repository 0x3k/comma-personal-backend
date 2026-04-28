// Command alpr-keygen prints a base64-encoded 32-byte random value
// suitable for use as the ALPR_ENCRYPTION_KEY environment variable.
//
// Usage:
//
//	go run ./cmd/alpr-keygen
//
// The output is a single line of standard (padded) base64. Treat the value
// as a high-value secret: it gates encryption and stable identity hashing
// for every plate read in the database, and rotating it invalidates ALL
// prior plate ciphertexts and hashes (data loss from the user's POV).
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintf(os.Stderr, "alpr-keygen: read random bytes: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(b))
}
