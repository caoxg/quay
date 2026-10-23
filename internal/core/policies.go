package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/caoxg/quay/internal/onnxinfo"
	"github.com/caoxg/quay/internal/policystore"
	"github.com/caoxg/quay/internal/profile"
	"github.com/caoxg/quay/internal/remote"
)

// InstalledPolicy is a model Quay put into one of an app's policy slots (state.json on the board).
type InstalledPolicy struct {
	ID          string `json:"id,omitempty"` // index entry id; empty for a local file
	Name        string `json:"name"`
	Author      string `json:"author,omitempty"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Index       string `json:"index,omitempty"`  // the index it came from
	Source      string `json:"source,omitempty"` // the file name, for a model installed from a file
	InstalledAt string `json:"installedAt,omitempty"`
}

// PolicySlot is one model slot of an installed app and what is in it now.
type PolicySlot struct {
	profile.Slot
	Active *InstalledPolicy `json:"active"` // nil: the app's stock model
	SHA256 string           `json:"sha256"` // the file in the slot now ("" if missing)
	// Changed: a policy is recorded but the file is something else (changed outside Quay).
	Changed bool `json:"changed"`
}

// PolicyStatus lists an app's policy slots.
type PolicyStatus struct {
	App     string       `json:"app"` // profile id
	Folder  string       `json:"folder"`
	Overlay bool         `json:"overlay"` // models go into an overlay folder (see profile.Policies)
	Slots   []PolicySlot `json:"slots"`
	Running bool         `json:"running"`
}

// PolicyCheck is what Quay found in a model before installing it.
type PolicyCheck struct {
	Slot     string          `json:"slot"`
	SHA256   string          `json:"sha256"`
	Size     int64           `json:"size"`
	Model    *onnxinfo.Model `json:"model"`
	Problems []string        `json:"problems"` // reasons not to install it; empty when it fits
	Same     bool            `json:"same"`     // the slot already holds this exact model
}

// InstallPolicyOptions say what to install.
type InstallPolicyOptions struct {
	Slot  string
	File  string          // the model on this computer (downloaded and verified, or picked by the user)
	Meta  InstalledPolicy // name, author, … recorded on the board; SHA256 and Size are filled in
	Force bool            // install despite failed checks and the app's safety probe
}

func (s *Session) policyApp(ctx context.Context, key string) (*App, *profile.Policies, error) {
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	if a.Profile == nil || a.Profile.Policies == nil {
		return a, nil, fmt.Errorf("%s does not declare policy slots (no policies: in its quay.yaml or built-in profile)", a.Name)
	}
	return a, a.Profile.Policies, nil
}

// policyScript runs remote.Policies with one request and returns its output.
func (s *Session) policyScript(ctx context.Context, a *App, p *profile.Policies, req map[string]any) (string, error) {
	slots := make([]map[string]string, len(p.Slots))
	for i, sl := range p.Slots {
		slots[i] = map[string]string{"id": sl.ID, "file": sl.File}
	}
	req["app"], req["dir"], req["overlay"], req["slots"] = a.Path, p.Dir, p.Overlay(), slots
	if p.StockDir != "" {
		req["stockDir"] = p.StockDir
	}
	b, _ := json.Marshal(req)
	var out, errb strings.Builder
	err := s.T.Exec(ctx, "python3 - "+base64.StdEncoding.EncodeToString(b), strings.NewReader(remote.Policies), &out, &errb)
	if err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			if i := strings.LastIndex(msg, "\n"); i >= 0 {
				msg = msg[i+1:]
			}
			return out.String(), errors.New(msg)
		}
		return out.String(), err
	}
	return out.String(), nil
}

// Policies lists an app's policy slots and what each holds.
func (s *Session) Policies(ctx context.Context, key string) (*PolicyStatus, error) {
	a, p, err := s.policyApp(ctx, key)
	if err != nil {
		return nil, err
	}
	out, err := s.policyScript(ctx, a, p, map[string]any{"op": "status"})
	if err != nil {
		return nil, err
	}
	var raw struct {
		Slots map[string]struct {
			SHA256 string           `json:"sha256"`
			Active *InstalledPolicy `json:"active"`
		} `json:"slots"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("policies: unexpected reply: %w", err)
	}
	ps := &PolicyStatus{App: a.Profile.ID, Folder: a.Folder, Overlay: p.Overlay(), Running: active(a.Status), Slots: []PolicySlot{}}
	for _, sl := range p.Slots {
		r := raw.Slots[sl.ID]
		x := PolicySlot{Slot: sl, Active: r.Active, SHA256: r.SHA256}
		x.Changed = r.Active != nil && r.SHA256 != r.Active.SHA256
		ps.Slots = append(ps.Slots, x)
	}
	return ps, nil
}

// stockModel reads the stock model of a slot from the board (nil when there is none to compare).
func (s *Session) stockModel(ctx context.Context, a *App, p *profile.Policies, slot string) *onnxinfo.Model {
	out, err := s.policyScript(ctx, a, p, map[string]any{"op": "stockpath", "slot": slot})
	path := strings.TrimSpace(out)
	if err != nil || path == "" {
		return nil
	}
	var buf bytes.Buffer
	if err := s.T.Download(ctx, path, &buf); err != nil {
		return nil
	}
	m, err := onnxinfo.Parse(buf.Bytes())
	if err != nil {
		return nil
	}
	return m
}

// CheckPolicy inspects a model on this computer against an app's slot. With slot "" it picks the
// slot from the file name when it matches one (e.g. xgoduck_walk.onnx).
func (s *Session) CheckPolicy(ctx context.Context, key, slot, file string) (*PolicyCheck, error) {
	a, p, err := s.policyApp(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.checkPolicy(ctx, a, p, slot, file)
}

func (s *Session) checkPolicy(ctx context.Context, a *App, p *profile.Policies, slot, file string) (*PolicyCheck, error) {
	if slot == "" {
		base := file[strings.LastIndexAny(file, `/\`)+1:]
		for _, sl := range p.Slots {
			if sl.File == base {
				slot = sl.ID
			}
		}
	}
	sl := p.Slot(slot)
	if sl == nil {
		ids := make([]string, len(p.Slots))
		for i, x := range p.Slots {
			ids[i] = x.ID
		}
		return nil, fmt.Errorf("%s has no policy slot %q (slots: %s)", a.Name, slot, strings.Join(ids, ", "))
	}
	sum, size, err := policystore.HashFile(file)
	if err != nil {
		return nil, err
	}
	c := &PolicyCheck{Slot: sl.ID, SHA256: sum, Size: size, Problems: []string{}}
	if c.Model, err = onnxinfo.ReadFile(file); err != nil {
		c.Problems = append(c.Problems, err.Error())
		return c, nil
	}
	c.Problems = append(c.Problems, policystore.Check(p, sl, c.Model, s.stockModel(ctx, a, p, sl.ID))...)
	if ps, err := s.Policies(ctx, a.Folder); err == nil {
		for _, x := range ps.Slots {
			c.Same = c.Same || (x.ID == sl.ID && x.SHA256 == sum)
		}
	}
	return c, nil
}

// InstallPolicy puts a model into one of an app's slots. A running app is stopped (after its
// safety check) and started again; if it does not come up with the new model, the previous one is
// put back and the app is started again.
func (s *Session) InstallPolicy(ctx context.Context, sink Sink, key string, opt InstallPolicyOptions) error {
	o := s.op(ctx, sink)
	a, p, err := s.policyApp(ctx, key)
	if err != nil {
		return err
	}
	var c *PolicyCheck
	if err := o.step("model", "Check the model", func() error {
		var err error
		if c, err = s.checkPolicy(ctx, a, p, opt.Slot, opt.File); err != nil {
			return err
		}
		if c.Model != nil {
			o.info(fmt.Sprintf("%s → %s: input %s, output %s", opt.Meta.Name, c.Slot, onnxinfo.ShapeString(c.Model.Inputs[0].Shape), onnxinfo.ShapeString(c.Model.Outputs[0].Shape)), c)
		}
		if len(c.Problems) == 0 {
			return nil
		}
		if opt.Force && c.Model != nil {
			for _, pr := range c.Problems {
				o.warn("installing anyway: " + pr)
			}
			return nil
		}
		return &PolicyError{Problems: c.Problems}
	}); err != nil {
		return err
	}
	sl := p.Slot(c.Slot)
	meta := opt.Meta
	meta.SHA256, meta.Size = c.SHA256, c.Size
	if meta.Name == "" {
		meta.Name = meta.Source
	}

	upload := "/tmp/quay-policy-" + randHex(6) + ".onnx"
	if err := o.step("upload", fmt.Sprintf("Upload %s (%s)", meta.Name, human(int(c.Size))), func() error {
		f, err := os.Open(opt.File)
		if err != nil {
			return err
		}
		defer f.Close()
		return s.T.Upload(ctx, f, upload)
	}); err != nil {
		return err
	}
	running := active(a.Status)
	if running {
		if err := o.step("safety", "Check it is safe to stop "+a.Name, func() error { return o.guardStop(a, opt.Force) }); err != nil {
			_, _ = o.quiet(remote.Script("rm -f "+upload, nil))
			return err
		}
		if err := o.step("stop", "Stop "+a.Name, func() error { return o.stopApp(a.Path) }); err != nil {
			_, _ = o.quiet(remote.Script("rm -f "+upload, nil))
			return err
		}
	}
	var prev *InstalledPolicy
	if err := o.step("switch", fmt.Sprintf("Put %s into %s (%s)", meta.Name, slotName(sl), sl.File), func() error {
		out, err := s.policyScript(ctx, a, p, map[string]any{"op": "apply", "slot": sl.ID, "upload": upload, "meta": meta})
		if err != nil {
			return err
		}
		var r struct {
			Previous *InstalledPolicy `json:"previous"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		prev = r.Previous
		return nil
	}); err != nil {
		if running {
			_ = o.step("start", "Start "+a.Name+" again", func() error { return o.startApp(a.Path, a.Profile) })
		}
		return err
	}
	if running {
		if err := o.step("start", "Start "+a.Name+" with the new model", func() error { return o.startApp(a.Path, a.Profile) }); err != nil {
			return s.rollbackPolicy(o, a, p, sl, prev, err)
		}
	} else {
		o.info(a.Name+" is not running; it loads the new model when it starts", nil)
	}
	_, _ = s.policyScript(ctx, a, p, map[string]any{"op": "gc"})
	return nil
}

func (s *Session) rollbackPolicy(o *op, a *App, p *profile.Policies, sl *profile.Slot, prev *InstalledPolicy, cause error) error {
	what := "the stock model"
	if prev != nil {
		what = prev.Name
	}
	if err := o.step("rollback", "Put "+what+" back", func() error {
		_, err := s.policyScript(o.ctx, a, p, map[string]any{"op": "rollback", "slot": sl.ID, "previous": prev})
		return err
	}); err != nil {
		return fmt.Errorf("%s did not start with the new model (%v), and putting %s back failed: %w", a.Name, cause, what, err)
	}
	_, _ = s.policyScript(o.ctx, a, p, map[string]any{"op": "gc"})
	if err := o.step("start", "Start "+a.Name+" again", func() error { return o.startApp(a.Path, a.Profile) }); err != nil {
		return fmt.Errorf("%s did not start with the new model (%v); %s is back, but the app did not start either: %w", a.Name, cause, what, err)
	}
	return fmt.Errorf("%s did not start with the new model, so %s is back: %w", a.Name, what, cause)
}

// RestorePolicies puts the stock models back into the given slots (all slots when none are given).
func (s *Session) RestorePolicies(ctx context.Context, sink Sink, key string, slots []string, force bool) error {
	o := s.op(ctx, sink)
	a, p, err := s.policyApp(ctx, key)
	if err != nil {
		return err
	}
	if len(slots) == 0 {
		for id := range a.Policies {
			slots = append(slots, id)
		}
	}
	var todo []string
	for _, id := range slots {
		if p.Slot(id) == nil {
			return fmt.Errorf("%s has no policy slot %q", a.Name, id)
		}
		if _, ok := a.Policies[id]; ok {
			todo = append(todo, id)
		}
	}
	if len(todo) == 0 {
		o.info("The stock models are already in place", nil)
		return nil
	}
	running := active(a.Status)
	if running {
		if err := o.step("safety", "Check it is safe to stop "+a.Name, func() error { return o.guardStop(a, force) }); err != nil {
			return err
		}
		if err := o.step("stop", "Stop "+a.Name, func() error { return o.stopApp(a.Path) }); err != nil {
			return err
		}
	}
	err = o.step("stock", "Put the stock models back: "+strings.Join(todo, ", "), func() error {
		_, err := s.policyScript(ctx, a, p, map[string]any{"op": "restore", "restore": todo})
		return err
	})
	_, _ = s.policyScript(ctx, a, p, map[string]any{"op": "gc"})
	if running {
		if serr := o.step("start", "Start "+a.Name+" again", func() error { return o.startApp(a.Path, a.Profile) }); err == nil {
			err = serr
		}
	}
	return err
}

// reapplyPolicies runs after a deploy replaced the app's code (and with it the stock models): it
// puts the installed policies back before the app starts.
func (o *op) reapplyPolicies(a *App, p *profile.Profile) error {
	return o.step("policies", "Put the installed policies back", func() error {
		if p == nil || p.Policies == nil {
			return errSkip
		}
		out, err := o.s.policyScript(o.ctx, a, p.Policies, map[string]any{"op": "reapply"})
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "QUAY_NONE" {
			return errSkip
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			o.log(l)
		}
		return nil
	})
}

// PolicyError lists why a model does not fit a slot.
type PolicyError struct{ Problems []string }

func (e *PolicyError) Error() string {
	return "this model does not fit: " + strings.Join(e.Problems, "; ")
}

func slotName(s *profile.Slot) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}
