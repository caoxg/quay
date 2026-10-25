// Package profile reads quay.yaml: the optional hooks an Arduino App declares so Quay can deploy
// it correctly (one-time system setup, steps before start, health and safety checks, uninstall).
// Apps without one are deployed generically. Built-in profiles cover known apps that do not ship
// a quay.yaml yet.
package profile

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/caoxg/quay/internal/hub"
)

// FileName is looked up at the app root.
const FileName = "quay.yaml"

// Profile is the parsed quay.yaml.
type Profile struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description,omitempty"`
	// Icon replaces the app.yaml icon in Quay: an emoji, or inline SVG markup (<svg …>…</svg>).
	Icon string `yaml:"icon" json:"icon,omitempty"`
	// Folder is the default install folder under ~/ArduinoApps.
	Folder string `yaml:"folder" json:"folder,omitempty"`
	// Source offers the app in the GUI's catalog (repo URL and optional ref/subdir).
	Source *Source `yaml:"source" json:"source,omitempty"`
	// Match lets a built-in profile recognise a repository or an installed app.
	Match Match `yaml:"match" json:"-"`

	System      *System      `yaml:"system" json:"system,omitempty"`
	BeforeStart *BeforeStart `yaml:"before_start" json:"beforeStart,omitempty"`
	Health      *Probe       `yaml:"health" json:"health,omitempty"`
	Safety      *Safety      `yaml:"safety" json:"safety,omitempty"`
	Web         *Web         `yaml:"web" json:"web,omitempty"`
	Uninstall   *Script      `yaml:"uninstall" json:"uninstall,omitempty"`
	Policies    *Policies    `yaml:"policies" json:"policies,omitempty"`
	Actions     []Action     `yaml:"actions" json:"actions,omitempty"`

	Origin string `yaml:"-" json:"origin"` // "repo" (quay.yaml in the app) or "builtin"
}

type Source struct {
	Repo   string `yaml:"repo" json:"repo"`
	Ref    string `yaml:"ref" json:"ref,omitempty"`
	Subdir string `yaml:"subdir" json:"subdir,omitempty"`
}

type Match struct {
	Repos    []string `yaml:"repos"`     // normalised repo URLs, e.g. github.com/owner/name
	AppNames []string `yaml:"app_names"` // the `name:` in app.yaml
}

// System is a one-time change that needs root (run by `quay prepare`).
type System struct {
	Description   string `yaml:"description" json:"description"`
	DescriptionZH string `yaml:"description_zh" json:"descriptionZh,omitempty"`
	Check         string `yaml:"check" json:"check"`   // exits 0 when already applied (runs as user)
	Apply         string `yaml:"apply" json:"apply"`   // runs as root in the app folder
	Revert        string `yaml:"revert" json:"revert"` // runs as root in the app folder
	StopApp       bool   `yaml:"stop_app" json:"stopApp"`
}

// BeforeStart runs as the user in the app folder after every deploy, before the app starts.
type BeforeStart struct {
	Run           string `yaml:"run" json:"run"`
	Description   string `yaml:"description" json:"description,omitempty"` // what it does, in a sentence
	DescriptionZH string `yaml:"description_zh" json:"descriptionZh,omitempty"`
	// ResolveOnFailure: if Run fails, start/stop the app once (App CLI resolves sketch.yaml
	// libraries on the first build) and run it again.
	ResolveOnFailure bool `yaml:"resolve_on_failure" json:"resolveOnFailure"`
	// RebuildWhenChanged: globs relative to $HOME; if their content changes during Run, the
	// sketch build cache is cleared so the firmware is rebuilt against the new files.
	RebuildWhenChanged []string `yaml:"rebuild_when_changed" json:"rebuildWhenChanged,omitempty"`
}

// Probe is an HTTP endpoint on the board that answers once the app is up.
type Probe struct {
	URL     string `yaml:"url" json:"url"`
	Timeout int    `yaml:"timeout" json:"timeout"` // seconds
}

// Safety refuses to stop/replace the app while a JSON field at URL is true, or while any of Also
// matches.
type Safety struct {
	URL     string        `yaml:"url" json:"url"`
	Field   string        `yaml:"field" json:"field"`
	Message string        `yaml:"message" json:"message"`
	Also    []SafetyCheck `yaml:"also" json:"also,omitempty"`
}

// SafetyCheck is one more reason to refuse: Field is true, or (with Equals) one of those values.
type SafetyCheck struct {
	Field   string   `yaml:"field" json:"field"`
	Equals  []string `yaml:"equals" json:"equals,omitempty"`
	Message string   `yaml:"message" json:"message"`
}

// Script is a shell script with an optional description; quay.yaml gives it as a plain string or
// as {run, description, description_zh}.
type Script struct {
	Run           string `yaml:"run" json:"run"`
	Description   string `yaml:"description" json:"description,omitempty"`
	DescriptionZH string `yaml:"description_zh" json:"descriptionZh,omitempty"`
}

func (s *Script) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&s.Run)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i]; k.Value {
		case "run", "description", "description_zh":
		default:
			return fmt.Errorf("line %d: field %s not found in script", k.Line, k.Value)
		}
	}
	type plain Script
	return n.Decode((*plain)(s))
}

// Action is a button on the app card: a page of the app's web console ("/calibrate.html") or a full
// http(s) URL, where {host} stands for the board's address.
type Action struct {
	Label       string `yaml:"label" json:"label"`
	LabelZH     string `yaml:"label_zh" json:"labelZh,omitempty"`
	URL         string `yaml:"url" json:"url"`
	Description string `yaml:"description" json:"description,omitempty"`
}

type Web struct {
	Port int    `yaml:"port" json:"port"`
	Path string `yaml:"path" json:"path,omitempty"`
}

// Policies declares the ONNX models (policies) an app loads, so Quay can install ones other people
// trained in their place, put the stock ones back, and keep the choice across updates.
type Policies struct {
	// Dir is the folder, relative to the app folder, the app loads its models from. A folder under
	// data/ is an overlay the app reads before its stock models; any other folder holds the stock
	// models, which Quay backs up before replacing and replaces again after every deploy.
	Dir string `yaml:"dir" json:"dir"`
	// StockDir is where the stock models are when Dir is an overlay (for the metadata comparison).
	StockDir string `yaml:"stock_dir" json:"stockDir,omitempty"`
	// Input and Output are the shapes every model must have (-1 matches any size).
	Input  []int64 `yaml:"input" json:"input,omitempty"`
	Output []int64 `yaml:"output" json:"output,omitempty"`
	// MatchMetadata names ONNX metadata keys whose values must equal the stock model's in the same
	// slot (e.g. joint_names: a model trained for another joint order would move the wrong servos).
	MatchMetadata []string `yaml:"match_metadata" json:"matchMetadata,omitempty"`
	Slots         []Slot   `yaml:"slots" json:"slots"`
	// Index is where the community publishes policies for this app; Quay reads it in addition to
	// the indexes the user added.
	Index *IndexSource `yaml:"index" json:"index,omitempty"`
}

// IndexSource is a policy index URL with optional mirrors (e.g. a CDN copy).
type IndexSource struct {
	URL     string   `yaml:"url" json:"url"`
	Mirrors []string `yaml:"mirrors" json:"mirrors,omitempty"`
}

// Slot is one model file the app loads.
type Slot struct {
	ID          string `yaml:"id" json:"id"`
	File        string `yaml:"file" json:"file"`
	Name        string `yaml:"name" json:"name"`
	NameZH      string `yaml:"name_zh" json:"nameZh,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
	// RequireMetadata names ONNX metadata keys a model for this slot must carry.
	RequireMetadata []string `yaml:"require_metadata" json:"requireMetadata,omitempty"`
}

// Overlay reports whether models go into a folder the app reads before its stock models (under
// data/, so deploys keep it), instead of replacing the stock files.
func (p *Policies) Overlay() bool {
	return p.Dir == "data" || strings.HasPrefix(p.Dir, "data/")
}

// Slot finds a slot by id.
func (p *Policies) Slot(id string) *Slot {
	for i := range p.Slots {
		if p.Slots[i].ID == id {
			return &p.Slots[i]
		}
	}
	return nil
}

func (p *Policies) validate() error {
	d := path.Clean(strings.TrimSpace(p.Dir))
	if p.Dir == "" || path.IsAbs(d) || d == "." || d == ".." || strings.HasPrefix(d, "../") {
		return errors.New("policies.dir must be a folder inside the app")
	}
	p.Dir = d
	if len(p.Slots) == 0 {
		return errors.New("policies needs at least one slot")
	}
	if p.Index != nil && !strings.HasPrefix(p.Index.URL, "https://") {
		return errors.New("policies.index.url must be an https URL")
	}
	seen := map[string]bool{}
	for _, s := range p.Slots {
		if !slotID.MatchString(s.ID) || seen[s.ID] {
			return fmt.Errorf("policies: slot id %q must be unique and use a-z, 0-9, - or _", s.ID)
		}
		seen[s.ID] = true
		if s.File == "" || s.File != path.Base(s.File) || !strings.HasSuffix(s.File, ".onnx") {
			return fmt.Errorf("policies: slot %s needs a file name ending in .onnx", s.ID)
		}
	}
	return nil
}

var slotID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Parse decodes and validates a quay.yaml.
func Parse(b []byte) (*Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if p.System != nil && strings.TrimSpace(p.System.Apply) == "" {
		return nil, fmt.Errorf("%s: system.apply is required when system is set", FileName)
	}
	if p.Safety != nil && (p.Safety.URL == "" || p.Safety.Field == "") {
		return nil, fmt.Errorf("%s: safety needs url and field", FileName)
	}
	for _, a := range p.Actions {
		if a.Label == "" || !(strings.HasPrefix(a.URL, "/") || strings.HasPrefix(a.URL, "http://") || strings.HasPrefix(a.URL, "https://")) {
			return nil, fmt.Errorf("%s: each action needs a label and a url (a path like /settings, or http(s)://…)", FileName)
		}
	}
	if err := checkIcon(p.Icon); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if p.Policies != nil {
		if err := p.Policies.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", FileName, err)
		}
	}
	if p.Health != nil && p.Health.Timeout <= 0 {
		p.Health.Timeout = 120
	}
	return &p, nil
}

// MaxIconSVG bounds an inline SVG icon; an icon needs a few KB.
const MaxIconSVG = 16 << 10

func checkIcon(icon string) error {
	icon = strings.TrimSpace(icon)
	switch {
	case strings.HasPrefix(icon, "<svg"):
		if len(icon) > MaxIconSVG || !strings.HasSuffix(icon, "</svg>") {
			return fmt.Errorf("icon: an SVG icon is one <svg>…</svg> element of at most %d KB", MaxIconSVG>>10)
		}
	case strings.ContainsAny(icon, "<>") || utf8.RuneCountInString(icon) > 8:
		return errors.New("icon: use an emoji or inline <svg> markup")
	}
	return nil
}

var (
	builtinMu   sync.Mutex
	builtinFrom *hub.Snapshot
	builtinList []*Profile
)

// Builtins returns the app profiles from the profiles repository (hub: the newest copy Quay has,
// built in or fetched), sorted by name. A file that does not read is replaced by the built-in copy
// of the same file, if any.
func Builtins() []*Profile {
	snap := hub.Get()
	builtinMu.Lock()
	defer builtinMu.Unlock()
	if snap == builtinFrom {
		return builtinList
	}
	var out []*Profile
	for _, f := range snap.Each(snap.Index.Apps) {
		p, err := Parse(f.Data)
		if err != nil && snap.Source != "built in" {
			if b, ok := hub.Builtin().Files[f.Name]; ok {
				p, err = Parse(b)
			}
		}
		if err != nil {
			continue
		}
		p.Origin = "builtin"
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	builtinFrom, builtinList = snap, out
	return out
}

// ByID finds a built-in profile.
func ByID(id string) *Profile {
	for _, p := range Builtins() {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Lookup picks the profile for an app: the app's own quay.yaml wins, then a built-in profile that
// matches the repository URL or the app.yaml name. It returns nil for a generic app.
func Lookup(repoQuayYAML []byte, repoURL, appName string) (*Profile, error) {
	if len(repoQuayYAML) > 0 {
		p, err := Parse(repoQuayYAML)
		if err != nil {
			return nil, err
		}
		p.Origin = "repo"
		return p, nil
	}
	norm := NormalizeRepo(repoURL)
	for _, p := range Builtins() {
		for _, r := range p.Match.Repos {
			if norm != "" && NormalizeRepo(r) == norm {
				return p, nil
			}
		}
	}
	for _, p := range Builtins() {
		for _, n := range p.Match.AppNames {
			if appName != "" && strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(appName)) {
				return p, nil
			}
		}
	}
	return nil, nil
}

// NormalizeRepo turns https://github.com/A/B.git, git@github.com:A/B and github.com/A/B into
// "github.com/a/b" so URLs can be compared.
func NormalizeRepo(u string) string {
	s := strings.TrimSpace(strings.ToLower(u))
	if s == "" {
		return ""
	}
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "git+ssh://"} {
		s = strings.TrimPrefix(s, p)
	}
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s[:at], "/") {
		s = s[at+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], "/") {
		rest := s[i+1:]
		if j := strings.Index(rest, "/"); j >= 0 && isDigits(rest[:j]) { // host:port/path
			rest = rest[j+1:]
		}
		s = s[:i] + "/" + rest
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	return s
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
