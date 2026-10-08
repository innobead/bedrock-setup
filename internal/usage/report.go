package usage

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
)

// User is someone with a budget in a month: tracked spend is spend whose owner tag is one of these.
type User struct {
	Email      string
	LimitUSD   float64
	TriggerUSD float64
	Pause      pause.Status
	PauseKnown bool
}

// UserRow is one line of the users table.
type UserRow struct {
	Email       string   `json:"email"`
	LimitUSD    float64  `json:"limit_usd"`
	TriggerUSD  float64  `json:"trigger_usd"`
	SpentUSD    float64  `json:"spent_usd"`
	UsedPercent float64  `json:"used_percent"`
	ForecastUSD *float64 `json:"forecast_usd"`
	Pause       string   `json:"pause"`
	PauseText   string   `json:"pause_text"`
}

// Caller is one untracked caller.
type Caller struct {
	Who      string  `json:"who"`
	UsedVia  string  `json:"used_via"`
	USD      float64 `json:"usd"`
	LastUsed string  `json:"last_used"`
}

// MonthReport is usage for one month.
type MonthReport struct {
	Month        string    `json:"month"`
	Current      bool      `json:"current"`
	Through      string    `json:"through,omitempty"`
	Note         string    `json:"note,omitempty"`
	Files        int       `json:"files"`
	Users        []UserRow `json:"users"`
	TrackedUSD   float64   `json:"tracked_usd"`
	Untracked    []Caller  `json:"untracked"`
	UntrackedUSD float64   `json:"untracked_usd"`
}

// MinUSD is the smallest caller total shown; smaller callers are left out of the list (not the total).
const MinUSD = 0.01

// Classify names a caller and how they called: the session name (an SSO user's email) or IAM user
// name, and "SSO role X", "personal role without owner tag: R", "role R" or "IAM user".
func Classify(arn, owner string) (who, via string) {
	parts := strings.Split(arn, "/")
	who = parts[len(parts)-1]
	switch {
	case strings.Contains(arn, ":assumed-role/") && len(parts) >= 3:
		role := parts[1]
		switch {
		case strings.HasPrefix(role, "AWSReservedSSO_"):
			ps := strings.TrimPrefix(role, "AWSReservedSSO_")
			if i := strings.LastIndex(ps, "_"); i > 0 {
				ps = ps[:i]
			}
			via = "SSO role " + ps
		case strings.HasPrefix(role, "bedrock-user-") && owner == "":
			via = "personal role without owner tag: " + role
		case strings.HasPrefix(role, "bedrock-user-"):
			via = "personal role without budget: " + role
		default:
			via = "role " + role
		}
	case strings.Contains(arn, ":user/"):
		via = "IAM user"
	default:
		via = arn
	}
	return who, via
}

type userSet map[string]User

func newUserSet(users []User) userSet {
	s := userSet{}
	for _, u := range users {
		s[strings.ToLower(u.Email)] = u
	}
	return s
}

func (s userSet) owner(l Line) (User, bool) {
	if l.Owner == "" {
		return User{}, false
	}
	u, ok := s[strings.ToLower(l.Owner)]
	return u, ok
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// MonthStart parses YYYY-MM.
func MonthStart(month string) (time.Time, error) {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return t, fmt.Errorf("month must be YYYY-MM (got %q)", month)
	}
	return t.UTC(), nil
}

// Summarize builds a month's report. For the current month (now inside the month) it adds a
// forecast; otherwise pause state is as of the end of the month.
func Summarize(month string, lines []Line, users []User, now time.Time) MonthReport {
	start, _ := MonthStart(month)
	end := pause.NextMonth(start)
	current := !now.Before(start) && now.Before(end)
	set := newUserSet(users)
	r := MonthReport{Month: month, Current: current, Users: []UserRow{}, Untracked: []Caller{}}
	if current {
		r.Through = now.UTC().Format("2006-01-02")
	}
	spent := map[string]float64{}
	type key struct{ who, via string }
	callers := map[key]*Caller{}
	last := map[key]time.Time{}
	for _, l := range lines {
		if u, ok := set.owner(l); ok {
			spent[strings.ToLower(u.Email)] += l.Cost
			r.TrackedUSD += l.Cost
			continue
		}
		who, via := Classify(l.Principal, l.Owner)
		k := key{who, via}
		c := callers[k]
		if c == nil {
			c = &Caller{Who: who, UsedVia: via}
			callers[k] = c
		}
		c.USD += l.Cost
		r.UntrackedUSD += l.Cost
		if l.Start.After(last[k]) {
			last[k] = l.Start
		}
	}
	stateTime := now
	if !current {
		stateTime = start
	}
	for _, u := range users {
		s := spent[strings.ToLower(u.Email)]
		row := UserRow{Email: u.Email, LimitUSD: u.LimitUSD, TriggerUSD: u.TriggerUSD, SpentUSD: round2(s)}
		if u.LimitUSD > 0 {
			row.UsedPercent = math.Round(s / u.LimitUSD * 100)
		}
		if u.PauseKnown {
			row.Pause, row.PauseText = string(u.Pause.State), u.Pause.Text(stateTime)
		} else {
			row.Pause, row.PauseText = "unknown", "unknown"
		}
		if current && u.Pause.State != pause.Paused && u.Pause.State != pause.Off {
			days := float64(end.Sub(start).Hours() / 24)
			f := math.Round(s / float64(now.UTC().Day()) * days)
			row.ForecastUSD = &f
		}
		r.Users = append(r.Users, row)
	}
	sort.Slice(r.Users, func(i, j int) bool { return r.Users[i].Email < r.Users[j].Email })
	for k, c := range callers {
		c.USD = round2(c.USD)
		if c.USD < MinUSD {
			continue
		}
		if t := last[k]; !t.IsZero() {
			c.LastUsed = t.Format("2006-01-02")
		}
		r.Untracked = append(r.Untracked, *c)
	}
	sortCallers(r.Untracked)
	r.TrackedUSD, r.UntrackedUSD = round2(r.TrackedUSD), round2(r.UntrackedUSD)
	return r
}

func sortCallers(c []Caller) {
	sort.Slice(c, func(i, j int) bool {
		if c[i].USD != c[j].USD {
			return c[i].USD > c[j].USD
		}
		if c[i].Who != c[j].Who {
			return c[i].Who < c[j].Who
		}
		return c[i].UsedVia < c[j].UsedVia
	})
}

// HasUntracked reports whether there is untracked spend worth following up (exit 2).
func (r MonthReport) HasUntracked() bool { return len(r.Untracked) > 0 }

func dollars(v float64) string {
	if v == math.Trunc(v) {
		return "$" + strconv.FormatInt(int64(v), 10)
	}
	return "$" + strconv.FormatFloat(v, 'f', 2, 64)
}

func cents(v float64) string { return "$" + strconv.FormatFloat(v, 'f', 2, 64) }

// Sections selects what Write prints.
type Sections struct{ Tracked, Untracked bool }

// Write prints the report as text.
func (r MonthReport) Write(w io.Writer, s Sections) {
	if r.Current {
		through, _ := time.Parse("2006-01-02", r.Through)
		fmt.Fprintf(w, "%s (to %s, billing lags up to ~16 h)\n", r.Month, through.Format("Jan 2"))
	} else {
		fmt.Fprintf(w, "%s (%s)\n", r.Month, firstNonEmpty(r.Note, "final"))
	}
	if s.Tracked {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if r.Current {
			fmt.Fprintln(tw, "USERS\tLIMIT\tSPENT\tUSED\tFORECAST\tPAUSE")
		} else {
			fmt.Fprintln(tw, "USERS\tLIMIT\tSPENT\tUSED\tPAUSE")
		}
		for _, u := range r.Users {
			used := fmt.Sprintf("%.0f%%", u.UsedPercent)
			if r.Current {
				fc := "-"
				if u.ForecastUSD != nil {
					fc = dollars(*u.ForecastUSD)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Email, dollars(u.LimitUSD), cents(u.SpentUSD), used, fc, u.PauseText)
			} else {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Email, dollars(u.LimitUSD), cents(u.SpentUSD), used, u.PauseText)
			}
		}
		if len(r.Users) == 0 {
			fmt.Fprintln(tw, "(no users)")
		}
		fmt.Fprintf(tw, "Tracked total\t\t%s\n", cents(r.TrackedUSD))
		_ = tw.Flush()
	}
	if s.Untracked {
		fmt.Fprintln(w)
		if !r.HasUntracked() {
			fmt.Fprintln(w, "Everyone used their personal role. Nothing to follow up.")
			return
		}
		fmt.Fprintln(w, "UNTRACKED (outside personal roles)")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "WHO\tUSED VIA\tUSD\tLAST USED")
		for _, c := range r.Untracked {
			fmt.Fprintf(tw, "%s\t%s\t%.2f\t%s\n", c.Who, c.UsedVia, c.USD, c.LastUsed)
		}
		fmt.Fprintf(tw, "Untracked total\t\t%s\t\n", cents(r.UntrackedUSD))
		_ = tw.Flush()
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// Series is one row of the multi-month table.
type Series struct {
	Who      string             `json:"who"`
	UsedVia  string             `json:"used_via,omitempty"`
	ByMonth  map[string]float64 `json:"by_month"`
	TotalUSD float64            `json:"total_usd"`
}

// MultiReport is usage over several months: one row per user and per untracked caller.
type MultiReport struct {
	Months         []string           `json:"months"`
	Users          []Series           `json:"users"`
	Untracked      []Series           `json:"untracked"`
	TrackedTotals  map[string]float64 `json:"tracked_totals"`
	UntrackedTotal map[string]float64 `json:"untracked_totals"`
}

// Combine builds the multi-month table from monthly reports (oldest first).
func Combine(reports []MonthReport) MultiReport {
	m := MultiReport{Users: []Series{}, Untracked: []Series{}, TrackedTotals: map[string]float64{}, UntrackedTotal: map[string]float64{}}
	users := map[string]*Series{}
	callers := map[[2]string]*Series{}
	for _, r := range reports {
		m.Months = append(m.Months, r.Month)
		m.TrackedTotals[r.Month] = r.TrackedUSD
		m.UntrackedTotal[r.Month] = r.UntrackedUSD
		for _, u := range r.Users {
			s := users[u.Email]
			if s == nil {
				s = &Series{Who: u.Email, ByMonth: map[string]float64{}}
				users[u.Email] = s
			}
			s.ByMonth[r.Month] = u.SpentUSD
			s.TotalUSD = round2(s.TotalUSD + u.SpentUSD)
		}
		for _, c := range r.Untracked {
			k := [2]string{c.Who, c.UsedVia}
			s := callers[k]
			if s == nil {
				s = &Series{Who: c.Who, UsedVia: c.UsedVia, ByMonth: map[string]float64{}}
				callers[k] = s
			}
			s.ByMonth[r.Month] = c.USD
			s.TotalUSD = round2(s.TotalUSD + c.USD)
		}
	}
	for _, s := range users {
		m.Users = append(m.Users, *s)
	}
	for _, s := range callers {
		m.Untracked = append(m.Untracked, *s)
	}
	sort.Slice(m.Users, func(i, j int) bool { return m.Users[i].Who < m.Users[j].Who })
	sort.Slice(m.Untracked, func(i, j int) bool {
		a, b := m.Untracked[i], m.Untracked[j]
		if a.TotalUSD != b.TotalUSD {
			return a.TotalUSD > b.TotalUSD
		}
		return a.Who+a.UsedVia < b.Who+b.UsedVia
	})
	return m
}

// HasUntracked reports untracked spend in any month.
func (m MultiReport) HasUntracked() bool { return len(m.Untracked) > 0 }

func seriesName(r Series) string {
	if r.UsedVia != "" {
		return r.Who + " (" + r.UsedVia + ")"
	}
	return r.Who
}

// Write prints the multi-month table.
func (m MultiReport) Write(w io.Writer, s Sections) {
	section := func(title string, rows []Series, totals map[string]float64, totalLabel string) {
		// Names left-aligned, numbers right-aligned.
		table := [][]string{append(append([]string{title}, m.Months...), "TOTAL")}
		for _, r := range rows {
			row := []string{seriesName(r)}
			for _, mo := range m.Months {
				if v, ok := r.ByMonth[mo]; ok {
					row = append(row, fmt.Sprintf("%.2f", v))
				} else {
					row = append(row, "-")
				}
			}
			table = append(table, append(row, fmt.Sprintf("%.2f", r.TotalUSD)))
		}
		total := []string{totalLabel}
		sum := 0.0
		for _, mo := range m.Months {
			total = append(total, fmt.Sprintf("%.2f", totals[mo]))
			sum += totals[mo]
		}
		table = append(table, append(total, fmt.Sprintf("%.2f", round2(sum))))
		widths := make([]int, len(table[0]))
		for _, row := range table {
			for i, c := range row {
				widths[i] = max(widths[i], utf8.RuneCountInString(c))
			}
		}
		for _, row := range table {
			var b strings.Builder
			for i, c := range row {
				gap := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
				if i == 0 {
					b.WriteString(c + gap)
				} else {
					b.WriteString("  " + gap + c)
				}
			}
			fmt.Fprintln(w, b.String())
		}
	}
	if s.Tracked {
		section("USERS", m.Users, m.TrackedTotals, "Tracked total")
	}
	if s.Untracked {
		if s.Tracked {
			fmt.Fprintln(w)
		}
		if !m.HasUntracked() {
			fmt.Fprintln(w, "Everyone used their personal role. Nothing to follow up.")
			return
		}
		section("UNTRACKED", m.Untracked, m.UntrackedTotal, "Untracked total")
	}
}

// Amount is one row of a breakdown.
type Amount struct {
	Key string  `json:"key"`
	USD float64 `json:"usd"`
}

// Detail is one person's month: their spend per day, per model and per way of calling.
type Detail struct {
	Month    string   `json:"month"`
	Email    string   `json:"email"`
	User     *UserRow `json:"user,omitempty"`
	TotalUSD float64  `json:"total_usd"`
	Days     []Amount `json:"days"`
	Models   []Amount `json:"models"`
	Callers  []Caller `json:"callers"`
}

// Person builds the detail for one email: rows with that owner tag and rows whose caller session is
// that email (for example an SSO user calling outside the personal role).
func Person(rep MonthReport, lines []Line, email string) Detail {
	d := Detail{Month: rep.Month, Email: email, Days: []Amount{}, Models: []Amount{}, Callers: []Caller{}}
	for i := range rep.Users {
		if strings.EqualFold(rep.Users[i].Email, email) {
			u := rep.Users[i]
			d.User = &u
		}
	}
	days, models := map[string]float64{}, map[string]float64{}
	type key struct{ who, via string }
	callers := map[key]*Caller{}
	for _, l := range lines {
		who, via := Classify(l.Principal, l.Owner)
		if !strings.EqualFold(l.Owner, email) && !strings.EqualFold(who, email) {
			continue
		}
		if strings.EqualFold(l.Owner, email) && d.User != nil {
			via = "personal role"
		}
		d.TotalUSD += l.Cost
		days[l.Start.UTC().Format("2006-01-02")] += l.Cost
		model := l.Model
		if model == "" {
			model = "(unknown)"
		}
		models[model] += l.Cost
		k := key{who, via}
		c := callers[k]
		if c == nil {
			c = &Caller{Who: who, UsedVia: via}
			callers[k] = c
		}
		c.USD += l.Cost
		if day := l.Start.UTC().Format("2006-01-02"); day > c.LastUsed {
			c.LastUsed = day
		}
	}
	d.TotalUSD = round2(d.TotalUSD)
	for k, v := range days {
		d.Days = append(d.Days, Amount{k, round2(v)})
	}
	sort.Slice(d.Days, func(i, j int) bool { return d.Days[i].Key < d.Days[j].Key })
	for k, v := range models {
		d.Models = append(d.Models, Amount{k, round2(v)})
	}
	sort.Slice(d.Models, func(i, j int) bool {
		if d.Models[i].USD != d.Models[j].USD {
			return d.Models[i].USD > d.Models[j].USD
		}
		return d.Models[i].Key < d.Models[j].Key
	})
	for _, c := range callers {
		c.USD = round2(c.USD)
		d.Callers = append(d.Callers, *c)
	}
	sortCallers(d.Callers)
	return d
}

// Write prints the detail.
func (d Detail) Write(w io.Writer) {
	fmt.Fprintf(w, "%s  %s\n", d.Email, d.Month)
	if d.User != nil {
		fmt.Fprintf(w, "Limit %s, spent %s (%.0f%%), %s\n", dollars(d.User.LimitUSD), cents(d.User.SpentUSD), d.User.UsedPercent, d.User.PauseText)
	} else {
		fmt.Fprintln(w, "Not a user with a budget: all of this is untracked.")
	}
	if d.TotalUSD == 0 && len(d.Days) == 0 {
		fmt.Fprintln(w, "\nNo spend this month.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nDAY\tUSD")
	for _, a := range d.Days {
		fmt.Fprintf(tw, "%s\t%.2f\n", a.Key, a.USD)
	}
	fmt.Fprintln(tw, "\nMODEL\tUSD")
	for _, a := range d.Models {
		fmt.Fprintf(tw, "%s\t%.2f\n", a.Key, a.USD)
	}
	fmt.Fprintln(tw, "\nUSED VIA\tUSD")
	for _, c := range d.Callers {
		fmt.Fprintf(tw, "%s\t%.2f\n", c.UsedVia, c.USD)
	}
	fmt.Fprintf(tw, "\nTotal\t%s\n", cents(d.TotalUSD))
	_ = tw.Flush()
}

// UsersFromSnapshot returns a past month's users, limits and pause state from its snapshot.
func UsersFromSnapshot(s *pause.Snapshot) []User {
	out := make([]User, 0, len(s.Users))
	for _, u := range s.Users {
		out = append(out, User{
			Email: u.Email, LimitUSD: u.LimitUSD, TriggerUSD: u.TriggerUSD, PauseKnown: true,
			Pause: pause.Status{State: u.State, PausedAt: u.PausedAt, By: u.By, Reason: u.Reason},
		})
	}
	return out
}
