// Command build compiles cmd/monthly-unpause for Lambda (provided.al2023, arm64) and writes a
// reproducible assets/bootstrap.zip. Run it with go generate from internal/lambdacode.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lambdacode:", err)
		os.Exit(1)
	}
}

func run() error {
	tmp, err := os.MkdirTemp("", "bedrock-lambda-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bin := filepath.Join(tmp, "bootstrap")
	cmd := exec.Command("go", "build", "-trimpath", "-tags", "lambda.norpc", "-ldflags", "-s -w -buildid=",
		"-o", bin, "../../cmd/monthly-unpause")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building cmd/monthly-unpause: %w", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "bootstrap", Method: zip.Deflate, Modified: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join("assets", "bootstrap.zip"), buf.Bytes(), 0o644)
}
