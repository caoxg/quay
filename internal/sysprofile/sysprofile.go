// Package sysprofile recognises board systems other than the stock Arduino image (Duck OS, …) and
// what they do differently, from system profiles: small YAML files published in the quay-profiles
// repository, with a copy built into Quay. A profile only describes things: files that identify
// the system, saved network profiles to look for, and which of Quay's own actions deals with a
// difference. It never carries commands, so a profile fetched from the network cannot make Quay
// run anything new on a board.
package sysprofile

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/caoxg/quay/internal/hub"
)

// Set is the system profiles in use and where they came from.
type Set struct {
	Profiles []*Profile
	Source   string // "built in", or the repository's index URL
	Updated  string
	Problems []string
}

// FromSnapshot reads the system profiles of a copy of the profiles repository. A file that does
// not read (a newer format, say) is replaced by the built-in copy of the same file, if any.
func FromSnapshot(s *hub.Snapshot) *Set {
	set := &Set{Source: s.Source, Updated: s.Index.Updated}
	for _, f := range s.Each(s.Index.Systems) {
		p, errs := Parse(f.Data)
		if p == nil && s.Source != "built in" {
			if b, ok := hub.Builtin().Files[f.Name]; ok {
				p, _ = Parse(b)
			}
		}
		for _, e := range errs {
			set.Problems = append(set.Problems, f.Name+": "+e.Error())
		}
		if p != nil {
			set.Profiles = append(set.Profiles, p)
		}
	}
	return set
}

// Current is the system profiles of hub.Current.
func Current(ctx context.Context) *Set { return FromSnapshot(hub.Current(ctx)) }

// Builtin is the system profiles built into Quay.
func Builtin() *Set { return FromSnapshot(hub.Builtin()) }

// Profile describes one board system.
type Profile struct {
	ID            string  `yaml:"id" json:"id"`
	Name          string  `yaml:"name" json:"name"`
	NameZh        string  `yaml:"name_zh" json:"nameZh,omitempty"`
	Vendor        string  `yaml:"vendor" json:"vendor,omitempty"`
	Homepage      string  `yaml:"homepage" json:"homepage,omitempty"`
	Description   string  `yaml:"description" json:"description,omitempty"`
	DescriptionZh string  `yaml:"description_zh" json:"descriptionZh,omitempty"`
	Detect        Detect  `yaml:"detect" json:"-"`
	Version       *Field  `yaml:"version" json:"-"`
	Base          *Field  `yaml:"base" json:"-"`
	Notes         []*Note `yaml:"notes" json:"-"`
	// Hotspot is the system's own hotspot, set up by its own tools (Duck Setup on Duck OS): Quay
	// shows whether it comes up by itself, since it then decides how the board is reached away
	// from Wi-Fi.
	Hotspot *OwnHotspot `yaml:"hotspot" json:"-"`
}

// OwnHotspot identifies a system's own hotspot among the saved NetworkManager profiles.
type OwnHotspot struct {
	NMProfile NMMatch `yaml:"nm_profile"`
}

// Detect identifies the system: every file listed exists.
type Detect struct {
	Files []string `yaml:"files"`
}

// Field reads a value from a JSON file on the board (a top-level key).
type Field struct {
	File string `yaml:"file"`
	JSON string `yaml:"json"`
}

// Note is one thing the system does differently from the stock image that the user should know,
// shown where it matters (ShowIn) while its condition holds.
type Note struct {
	ID     string  `yaml:"id"`
	ShowIn string  `yaml:"show_in"` // network | overview
	When   When    `yaml:"when"`
	Text   string  `yaml:"text"`
	TextZh string  `yaml:"text_zh"`
	Action *Action `yaml:"action"`
}

// When is a note's condition: all the parts given hold. With none, the note always shows.
type When struct {
	Files     []string `yaml:"files"`      // these files exist
	NMProfile *NMMatch `yaml:"nm_profile"` // a saved NetworkManager profile matches
}

// NMMatch matches saved NetworkManager profiles on the fields given.
type NMMatch struct {
	ID          string `yaml:"id"`
	IDPrefix    string `yaml:"id_prefix"`
	SSID        string `yaml:"ssid"`
	Mode        string `yaml:"mode"` // infrastructure | ap
	Autoconnect *bool  `yaml:"autoconnect"`
}

// Action is one of Quay's built-in fixes, applied to what the note's condition matched.
type Action struct {
	Do        string `yaml:"do"`
	Label     string `yaml:"label"`
	LabelZh   string `yaml:"label_zh"`
	Confirm   string `yaml:"confirm"`
	ConfirmZh string `yaml:"confirm_zh"`
}

// Built-in actions. Each works on the NetworkManager profiles a note's nm_profile matched.
const (
	DoNMAutoconnectOff = "nm_autoconnect_off" // keep the profile, stop it coming up by itself
	DoNMDelete         = "nm_delete"          // delete the profile (never the one in use)
)

var (
	idRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	pathRE = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]{1,200}$`)
	keyRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// Parse reads and checks one profile. Notes with a problem are dropped and reported, so one bad
// note does not hide the rest; an action this Quay does not know is dropped from its note (a newer
// profile may name one), the note itself stays.
func Parse(b []byte) (*Profile, []error) {
	var p Profile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, []error{err}
	}
	if !idRE.MatchString(p.ID) {
		return nil, []error{fmt.Errorf("system profile id %q: lowercase letters, digits and -", p.ID)}
	}
	if p.Name == "" {
		return nil, []error{fmt.Errorf("%s: no name", p.ID)}
	}
	if len(p.Detect.Files) == 0 {
		return nil, []error{fmt.Errorf("%s: detect.files is empty", p.ID)}
	}
	for _, f := range p.Detect.Files {
		if err := checkPath(f); err != nil {
			return nil, []error{fmt.Errorf("%s: detect: %w", p.ID, err)}
		}
	}
	var errs []error
	for _, fld := range []**Field{&p.Version, &p.Base} {
		if f := *fld; f != nil {
			if err := checkPath(f.File); err != nil || !keyRE.MatchString(f.JSON) {
				errs = append(errs, fmt.Errorf("%s: bad field %+v", p.ID, *f))
				*fld = nil
			}
		}
	}
	if h := p.Hotspot; h != nil {
		if m := h.NMProfile; m.ID == "" && m.IDPrefix == "" && m.SSID == "" {
			errs = append(errs, fmt.Errorf("%s: hotspot.nm_profile needs an id, id_prefix or ssid", p.ID))
			p.Hotspot = nil
		}
	}
	var notes []*Note
	seen := map[string]bool{}
	for _, n := range p.Notes {
		if err := checkNote(n); err != nil {
			errs = append(errs, fmt.Errorf("%s: note %q: %w", p.ID, n.ID, err))
			continue
		}
		if seen[n.ID] {
			errs = append(errs, fmt.Errorf("%s: note %q twice", p.ID, n.ID))
			continue
		}
		seen[n.ID] = true
		if n.Action != nil && n.Action.Do != DoNMAutoconnectOff && n.Action.Do != DoNMDelete {
			errs = append(errs, fmt.Errorf("%s: note %q: action %q is not known to this Quay", p.ID, n.ID, n.Action.Do))
			n.Action = nil
		}
		notes = append(notes, n)
	}
	p.Notes = notes
	return &p, errs
}

func checkPath(p string) error {
	if !pathRE.MatchString(p) || path.Clean(p) != p || strings.Contains(p, "..") {
		return fmt.Errorf("path %q: an absolute, clean path", p)
	}
	return nil
}

func checkNote(n *Note) error {
	switch {
	case n == nil:
		return errors.New("empty")
	case !idRE.MatchString(n.ID):
		return errors.New("id: lowercase letters, digits and -")
	case n.ShowIn != "network" && n.ShowIn != "overview":
		return fmt.Errorf("show_in %q: network or overview", n.ShowIn)
	case n.Text == "":
		return errors.New("no text")
	}
	for _, f := range n.When.Files {
		if err := checkPath(f); err != nil {
			return err
		}
	}
	if m := n.When.NMProfile; m != nil && m.ID == "" && m.IDPrefix == "" && m.SSID == "" && m.Mode == "" && m.Autoconnect == nil {
		return errors.New("nm_profile matches nothing in particular")
	}
	if n.Action != nil && n.When.NMProfile == nil {
		return errors.New("an action needs an nm_profile to work on")
	}
	return nil
}

// ---------------------------------------------------------------- facts and results

// Facts is what Quay read on the board for the profiles.
type Facts struct {
	Files    map[string]FileFact // by path
	Profiles []NMProfile         // saved NetworkManager profiles (nil: not read)
}

// FileFact is one file: whether it exists, and its top-level JSON values when it was read as JSON.
type FileFact struct {
	Exists bool           `json:"exists"`
	JSON   map[string]any `json:"json,omitempty"`
}

// NMProfile is a saved NetworkManager profile.
type NMProfile struct {
	UUID        string
	ID          string
	SSID        string
	Mode        string
	Autoconnect bool
	Active      bool
}

// Checks lists what to read on the board for these profiles: files whose existence matters, and
// the JSON keys to read from files (only those, so nothing else in them leaves the board).
func Checks(ps []*Profile) (exists []string, keys map[string][]string) {
	keys = map[string][]string{}
	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	for _, p := range ps {
		files := append([]string{}, p.Detect.Files...)
		for _, n := range p.Notes {
			files = append(files, n.When.Files...)
		}
		for _, f := range files {
			if !has(exists, f) {
				exists = append(exists, f)
			}
		}
		for _, f := range []*Field{p.Version, p.Base} {
			if f != nil && !has(keys[f.File], f.JSON) {
				keys[f.File] = append(keys[f.File], f.JSON)
			}
		}
	}
	return exists, keys
}

// Detected is a recognised system, for display.
type Detected struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	NameZh        string `json:"nameZh,omitempty"`
	Version       string `json:"version,omitempty"`
	Base          string `json:"base,omitempty"`
	Vendor        string `json:"vendor,omitempty"`
	Homepage      string `json:"homepage,omitempty"`
	Description   string `json:"description,omitempty"`
	DescriptionZh string `json:"descriptionZh,omitempty"`
}

// Identify returns the first profile whose files all exist, or nil (the stock image, or a system
// Quay has no profile for).
func Identify(ps []*Profile, f Facts) *Profile {
	for _, p := range ps {
		ok := true
		for _, file := range p.Detect.Files {
			ok = ok && f.Files[file].Exists
		}
		if ok {
			return p
		}
	}
	return nil
}

// Info describes a detected system.
func (p *Profile) Info(f Facts) *Detected {
	return &Detected{ID: p.ID, Name: p.Name, NameZh: p.NameZh, Version: f.value(p.Version), Base: f.value(p.Base),
		Vendor: p.Vendor, Homepage: p.Homepage, Description: p.Description, DescriptionZh: p.DescriptionZh}
}

func (f Facts) value(fld *Field) string {
	if fld == nil {
		return ""
	}
	switch v := f.Files[fld.File].JSON[fld.JSON].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// HotspotState is a system's own hotspot found on the board.
type HotspotState struct {
	System      string `json:"system"` // the system's name
	SystemZh    string `json:"systemZh,omitempty"`
	SSID        string `json:"ssid"`
	Autoconnect bool   `json:"autoconnect"` // comes up by itself (at boot, ahead of Wi-Fi)
	Active      bool   `json:"active"`
}

// OwnHotspotState finds the system's own hotspot among the saved profiles (nil: none, or the
// profiles were not read). With several, one that comes up by itself wins.
func (p *Profile) OwnHotspotState(f Facts) *HotspotState {
	if p.Hotspot == nil {
		return nil
	}
	var out *HotspotState
	for _, nm := range f.Profiles {
		if !p.Hotspot.NMProfile.matches(nm) {
			continue
		}
		if out == nil || (nm.Autoconnect && !out.Autoconnect) {
			out = &HotspotState{System: p.Name, SystemZh: p.NameZh, SSID: nm.SSID, Autoconnect: nm.Autoconnect, Active: nm.Active}
		}
	}
	return out
}

// Finding is a note whose condition holds on this board.
type Finding struct {
	System string         `json:"system"`
	ID     string         `json:"id"`
	ShowIn string         `json:"showIn"`
	Text   string         `json:"text"`
	TextZh string         `json:"textZh,omitempty"`
	Action *ActionInfo    `json:"action,omitempty"`
	Match  []string       `json:"-"` // uuids of the NetworkManager profiles it matched
	Vars   map[string]any `json:"vars,omitempty"`
}

// ActionInfo is a finding's fix, for display.
type ActionInfo struct {
	Do        string `json:"do"`
	Label     string `json:"label"`
	LabelZh   string `json:"labelZh,omitempty"`
	Confirm   string `json:"confirm,omitempty"`
	ConfirmZh string `json:"confirmZh,omitempty"`
}

// Findings evaluates the profile's notes for one place (showIn; "" for all). A note that needs
// NetworkManager profiles is skipped when they were not read.
func (p *Profile) Findings(f Facts, showIn string) []Finding {
	var out []Finding
	for _, n := range p.Notes {
		if showIn != "" && n.ShowIn != showIn {
			continue
		}
		ok := true
		for _, file := range n.When.Files {
			ok = ok && f.Files[file].Exists
		}
		if !ok {
			continue
		}
		vars := map[string]any{}
		var match []string
		if m := n.When.NMProfile; m != nil {
			if f.Profiles == nil {
				continue
			}
			for _, nm := range f.Profiles {
				if m.matches(nm) {
					if len(match) == 0 {
						vars["ssid"], vars["id"] = nm.SSID, nm.ID
					}
					match = append(match, nm.UUID)
				}
			}
			if len(match) == 0 {
				continue
			}
			vars["count"] = len(match)
		}
		fd := Finding{System: p.ID, ID: n.ID, ShowIn: n.ShowIn, Text: fill(n.Text, vars), TextZh: fill(n.TextZh, vars), Match: match, Vars: vars}
		if a := n.Action; a != nil {
			fd.Action = &ActionInfo{Do: a.Do, Label: fill(a.Label, vars), LabelZh: fill(a.LabelZh, vars), Confirm: fill(a.Confirm, vars), ConfirmZh: fill(a.ConfirmZh, vars)}
		}
		out = append(out, fd)
	}
	return out
}

func (m *NMMatch) matches(p NMProfile) bool {
	return (m.ID == "" || p.ID == m.ID) && (m.IDPrefix == "" || strings.HasPrefix(p.ID, m.IDPrefix)) &&
		(m.SSID == "" || p.SSID == m.SSID) && (m.Mode == "" || p.Mode == m.Mode) &&
		(m.Autoconnect == nil || p.Autoconnect == *m.Autoconnect)
}

func fill(s string, vars map[string]any) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", fmt.Sprint(v))
	}
	return s
}
