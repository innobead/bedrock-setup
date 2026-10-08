package cli

import (
	"fmt"
	"io"
)

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// DoctorJSON is the doctor --json output.
type DoctorJSON struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// PrintChecks prints doctor results and returns Exit(Problem) when one failed.
func PrintChecks(w io.Writer, checks []Check, asJSON, quiet bool) error {
	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	if asJSON {
		if checks == nil {
			checks = []Check{}
		}
		if err := WriteJSON(w, DoctorJSON{OK: ok, Checks: checks}); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			if c.OK && quiet {
				continue
			}
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(w, "%s  %s", mark, c.Name)
			if c.Detail != "" {
				fmt.Fprintf(w, ": %s", c.Detail)
			}
			fmt.Fprintln(w)
			if !c.OK && c.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", c.Fix)
			}
		}
		if ok {
			fmt.Fprintf(w, "\nAll checks passed.\n")
		}
	}
	if !ok {
		return Exit(Problem)
	}
	return nil
}
