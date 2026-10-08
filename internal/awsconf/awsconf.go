// Package awsconf reads and edits the AWS CLI config file (~/.aws/config) line by line, so that
// everything outside the edited profile stays exactly as it was.
package awsconf

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Path returns the config file the AWS CLI and SDKs use: $AWS_CONFIG_FILE, else ~/.aws/config.
func Path() (string, error) {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aws", "config"), nil
}

// Section is one [header] block. Start is the header line; End is one past its last key line
// (trailing blank and comment lines belong to the next section).
type Section struct {
	Header     string // text between the brackets, trimmed
	Start, End int
	Keys       map[string]string
}

// Profile returns the profile name of a section header, or "" when it is not a profile
// ([default] is "default"; [sso-session x] and [services x] are not profiles).
func (s *Section) Profile() string {
	if s.Header == "default" {
		return "default"
	}
	if name, ok := strings.CutPrefix(s.Header, "profile "); ok {
		return strings.TrimSpace(name)
	}
	return ""
}

// IsSignIn reports whether the profile signs in with SSO (aws sso login) or aws login.
func (s *Section) IsSignIn() bool {
	return s.Keys["sso_session"] != "" || s.Keys["sso_start_url"] != "" || s.Keys["login_session"] != ""
}

// File is a parsed config file.
type File struct {
	Lines    []string
	Sections []*Section
}

// Read parses path; a missing file is an empty File.
func Read(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Parse(""), nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(string(b)), nil
}

// Parse parses config file text.
func Parse(text string) *File {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	f := &File{}
	if text != "" {
		f.Lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	var cur *Section
	for i, line := range f.Lines {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			cur = &Section{Header: strings.TrimSpace(t[1 : len(t)-1]), Start: i, End: i + 1, Keys: map[string]string{}}
			f.Sections = append(f.Sections, cur)
		case cur == nil || t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";"):
		default:
			cur.End = i + 1
			indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
			if k, v, ok := strings.Cut(t, "="); ok && !indented {
				cur.Keys[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return f
}

// Profile returns the section of a profile, or nil.
func (f *File) Profile(name string) *Section {
	for _, s := range f.Sections {
		if s.Profile() == name {
			return s
		}
	}
	return nil
}

// SignInProfiles returns the names of the profiles that sign in with SSO or aws login.
func (f *File) SignInProfiles() []string {
	var out []string
	for _, s := range f.Sections {
		if p := s.Profile(); p != "" && s.IsSignIn() {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// KV is one key = value line.
type KV struct{ Key, Value string }

// Block renders a profile block, without inline comments.
func Block(name string, kvs []KV) []string {
	h := "[profile " + name + "]"
	if name == "default" {
		h = "[default]"
	}
	out := []string{h}
	for _, kv := range kvs {
		out = append(out, kv.Key+" = "+kv.Value)
	}
	return out
}

// SetProfile replaces the profile's block, or appends it after a blank line.
func (f *File) SetProfile(name string, kvs []KV) {
	block := Block(name, kvs)
	var lines []string
	if s := f.Profile(name); s != nil {
		lines = append(lines, f.Lines[:s.Start]...)
		lines = append(lines, block...)
		lines = append(lines, f.Lines[s.End:]...)
	} else {
		lines = append(lines, f.Lines...)
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, block...)
	}
	*f = *Parse(strings.Join(lines, "\n"))
}

// String returns the file text, ending in a newline.
func (f *File) String() string {
	if len(f.Lines) == 0 {
		return ""
	}
	return strings.Join(f.Lines, "\n") + "\n"
}

// Write saves the file. When the file already exists, its previous content is first copied to
// path + ".bak".
func (f *File) Write(path string) (backup string, err error) {
	mode := os.FileMode(0o600)
	if old, err := os.ReadFile(path); err == nil {
		backup = path + ".bak"
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return "", fmt.Errorf("writing backup: %w", err)
		}
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(f.String()), mode); err != nil {
		return "", err
	}
	return backup, os.Rename(tmp, path)
}
