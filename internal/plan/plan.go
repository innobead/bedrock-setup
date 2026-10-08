// Package plan holds the list of changes plan shows and apply makes.
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

type Op string

const (
	OK       Op = "ok"
	Create   Op = "create"
	Update   Op = "update"
	Delete   Op = "delete"
	Handoff  Op = "needs-org-admin"
	Conflict Op = "conflict"
	// Info is shown but is not a change (for example "waiting for first use", "not managed").
	Info Op = "info"
)

// Pending reports whether an op counts as a change (plan exits 2).
func (o Op) Pending() bool {
	switch o {
	case Create, Update, Delete, Handoff, Conflict:
		return true
	}
	return false
}

func (o Op) symbol() string {
	switch o {
	case Create:
		return "+"
	case Update:
		return "~"
	case Delete:
		return "-"
	case Handoff:
		return "?"
	case Conflict:
		return "!"
	}
	return " "
}

// Item is one line of the plan: an account setup step or a user.
type Item struct {
	Section string   `json:"section"` // "account" or "users"
	Step    string   `json:"step,omitempty"`
	Target  string   `json:"target"`
	Op      Op       `json:"op"`
	Status  string   `json:"status,omitempty"`
	Details []string `json:"details,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	// Warnings are shown with the item and don't change the exit code.
	Warnings []string `json:"warnings,omitempty"`
	NeedsYes bool     `json:"needs_yes,omitempty"`

	// Users only.
	Email        string `json:"email,omitempty"`
	Pause        string `json:"pause,omitempty"`
	SetupCommand string `json:"setup_command,omitempty"`

	Apply func(context.Context) error `json:"-"`
}

// Plan is the full list of items.
type Plan struct {
	Header string  `json:"-"`
	Items  []*Item `json:"items"`
}

func (p *Plan) Add(items ...*Item) { p.Items = append(p.Items, items...) }

// Changes is the number of pending items.
func (p *Plan) Changes() int {
	n := 0
	for _, it := range p.Items {
		if it.Op.Pending() {
			n++
		}
	}
	return n
}

// Summary is the last line of plan, for example "Plan: 1 to create, 2 to change."
func (p *Plan) Summary() string {
	counts := map[Op]int{}
	for _, it := range p.Items {
		counts[it.Op]++
	}
	var parts []string
	for _, c := range []struct {
		op   Op
		text string
	}{{Create, "to create"}, {Update, "to change"}, {Delete, "to remove"}, {Handoff, "needs org admin"}, {Conflict, "conflict"}} {
		if n := counts[c.op]; n > 0 {
			t := c.text
			if c.op == Conflict && n > 1 {
				t = "conflicts"
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, t))
		}
	}
	if len(parts) == 0 {
		return "No changes."
	}
	return "Plan: " + strings.Join(parts, ", ") + "."
}

func (it *Item) text() string {
	var b strings.Builder
	switch {
	case len(it.Details) > 0:
		b.WriteString(strings.Join(it.Details, ", "))
	case it.Status != "":
		b.WriteString(it.Status)
	default:
		b.WriteString(string(it.Op))
	}
	if len(it.Notes) > 0 {
		b.WriteString("  (" + strings.Join(it.Notes, "; ") + ")")
	}
	if it.NeedsYes {
		b.WriteString("  (needs --yes)")
	}
	return b.String()
}

// Render writes the plan as text. With quiet, unchanged items are left out.
func Render(w io.Writer, p *Plan, quiet bool) {
	if p.Header != "" {
		_, _ = fmt.Fprintln(w, p.Header)
	}
	for _, section := range []string{"account", "users"} {
		var items []*Item
		for _, it := range p.Items {
			if it.Section == section && (!quiet || it.Op.Pending() || len(it.Warnings) > 0) {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			continue
		}
		title := "Account setup"
		if section == "users" {
			title = "Users"
		}
		_, _ = fmt.Fprintf(w, "\n%s\n", title)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, it := range items {
			label := it.Target
			if it.Step != "" {
				label = it.Step + " " + it.Target
			}
			line := it.text()
			if section == "users" && it.Pause != "" && !it.Op.Pending() {
				line += "\t" + it.Pause
			}
			_, _ = fmt.Fprintf(tw, "%s %s\t%s\n", it.Op.symbol(), label, line)
			for _, warn := range it.Warnings {
				// No tab: a warning isn't a cell, so it doesn't widen the label column.
				_, _ = fmt.Fprintf(tw, "    warning: %s\n", warn)
			}
		}
		_ = tw.Flush()
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, p.Summary())
}

// JSON is the --json form of a plan.
type JSON struct {
	Changes int     `json:"changes"`
	Summary string  `json:"summary"`
	Items   []*Item `json:"items"`
}

func RenderJSON(w io.Writer, p *Plan) error {
	items := p.Items
	if items == nil {
		items = []*Item{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(JSON{Changes: p.Changes(), Summary: p.Summary(), Items: items})
}
