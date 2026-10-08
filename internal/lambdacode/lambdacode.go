// Package lambdacode embeds the monthly unpause Lambda (cmd/monthly-unpause) so apply can deploy it.
package lambdacode

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
)

//go:generate go run ./build

//go:embed assets
var assets embed.FS

// ErrMissing means the binary was built without running go generate first.
var ErrMissing = errors.New("this bedrock-admin build has no Lambda code; build it with `make` (runs go generate)")

// Zip returns the deployment package.
func Zip() ([]byte, error) {
	b, err := assets.ReadFile("assets/bootstrap.zip")
	if err != nil || len(b) == 0 {
		return nil, ErrMissing
	}
	return b, nil
}

// Sha256 is the package hash in the form Lambda reports as CodeSha256.
func Sha256(zip []byte) string {
	sum := sha256.Sum256(zip)
	return base64.StdEncoding.EncodeToString(sum[:])
}
