// Package cli holds what both CLIs share: exit codes, prompts and JSON output.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Exit codes.
const (
	OK      = 0
	Error   = 1
	Problem = 2 // ran fine but found something: plan has changes, doctor found an issue, untracked spend
)

// ExitError ends the command with a code. Err, when set, is printed as the error.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Exit returns an error that ends the command with code and prints nothing.
func Exit(code int) error {
	if code == OK {
		return nil
	}
	return &ExitError{Code: code}
}

// Run executes the root command and returns the process exit code.
func Run(ctx context.Context, root *cobra.Command, stderr io.Writer) int {
	root.SilenceErrors, root.SilenceUsage = true, true
	err := root.ExecuteContext(ctx)
	if err == nil {
		return OK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if ee.Err != nil {
			_, _ = fmt.Fprintln(stderr, "error:", ee.Err)
		}
		return ee.Code
	}
	_, _ = fmt.Fprintln(stderr, "error:", err)
	if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown flag") ||
		strings.Contains(err.Error(), "accepts ") || strings.HasPrefix(err.Error(), "required flag") {
		_, _ = fmt.Fprintln(stderr, "Run with --help for usage.")
	}
	return Error
}

// WriteJSON writes v as indented JSON.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// IsTerminal reports whether r is an interactive terminal.
func IsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Prompter asks questions on a terminal.
type Prompter struct {
	In  *bufio.Reader
	Out io.Writer
}

func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{In: bufio.NewReader(in), Out: out}
}

// Ask prints a question with a default and returns the answer (or the default).
func (p *Prompter) Ask(question, def string) (string, error) {
	if def != "" {
		_, _ = fmt.Fprintf(p.Out, "%s [%s]: ", question, def)
	} else {
		_, _ = fmt.Fprintf(p.Out, "%s: ", question)
	}
	line, err := p.In.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", fmt.Errorf("reading answer: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// Confirm asks a yes/no question; the default is no.
func (p *Prompter) Confirm(question string) (bool, error) {
	a, err := p.Ask(question+" (y/N)", "")
	if err != nil {
		return false, err
	}
	a = strings.ToLower(a)
	return a == "y" || a == "yes", nil
}
