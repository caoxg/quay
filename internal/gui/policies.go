package gui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/caoxg/quay/internal/config"
	"github.com/caoxg/quay/internal/core"
	"github.com/caoxg/quay/internal/policystore"
	"github.com/caoxg/quay/internal/profile"
)

// PolicyCatalog is what the policy dialog lists for one app profile.
type PolicyCatalog struct {
	Entries  []policystore.Entry `json:"entries"`
	Problems []string            `json:"problems"`
	Indexes  []string            `json:"indexes"` // the community index first, then the user's
	Sources  []CatalogSource     `json:"sources"` // the same indexes, named for the dialog
}

// CatalogSource names one index the catalog was read from.
type CatalogSource struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Page string `json:"page,omitempty"`
}

// PolicyRequest names a policy to check or install: an index entry, or a model file on this computer.
type PolicyRequest struct {
	Board  string             `json:"board"`
	Folder string             `json:"folder"`
	Entry  *policystore.Entry `json:"entry,omitempty"`
	File   string             `json:"file,omitempty"`
	Slot   string             `json:"slot"`
	Name   string             `json:"name,omitempty"`
	Author string             `json:"author,omitempty"`
	Force  bool               `json:"force"`
}

// PolicyChecked is a check's result plus what was checked.
type PolicyChecked struct {
	*core.PolicyCheck
	File string `json:"file"`
}

func (s *Service) policyStore() *policystore.Store {
	return &policystore.Store{Dir: config.PolicyCacheDir()}
}

// PolicyCatalog reads the app profile's community index and the user's own indexes, and keeps the
// entries for that profile. index is the profile's policies.index (the app's own quay.yaml may
// name one that no built-in profile knows).
func (s *Service) PolicyCatalog(app string, index *profile.IndexSource) string {
	return s.run("policy-catalog", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		src := policystore.Sources([]*profile.Profile{{Policies: &profile.Policies{Index: index}}}, cfg.PolicyIndexes)
		urls := make([]string, len(src))
		named := make([]CatalogSource, len(src))
		for i, x := range src {
			urls[i] = x.URL
			named[i] = CatalogSource{URL: x.URL}
			named[i].Name, named[i].Page = policystore.Describe(x.URL)
		}
		r := s.policyStore().Load(ctx, src)
		return PolicyCatalog{Entries: policystore.ForApp(r.Entries, app), Problems: r.Problems, Indexes: urls, Sources: named}, nil
	})
}

// Policies lists an installed app's policy slots.
func (s *Service) Policies(boardName, folder string) string {
	return s.run("policies", boardName, folder, func(ctx context.Context, sess *core.Session, _ core.Sink) (any, error) {
		return sess.Policies(ctx, folder)
	})
}

// policyFile returns the model to install: a downloaded and verified index entry, or the user's file.
func (s *Service) policyFile(ctx context.Context, sink core.Sink, req PolicyRequest) (string, error) {
	if req.Entry == nil {
		if req.File == "" {
			return "", errors.New("no policy given")
		}
		return req.File, nil
	}
	e := *req.Entry
	text := "Download " + e.Name
	sink(core.Event{Type: "step", Step: "download", State: "run", Text: text})
	f, err := s.policyStore().Download(ctx, e, nil)
	if err != nil {
		sink(core.Event{Type: "step", Step: "download", State: "fail", Text: err.Error()})
		return "", err
	}
	sink(core.Event{Type: "step", Step: "download", State: "ok", Text: text + " (checksum verified)"})
	return f, nil
}

// CheckPolicy downloads (for an index entry) and checks a model against an app's slot.
func (s *Service) CheckPolicy(req PolicyRequest) string {
	return s.run("policy-check", req.Board, req.Folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		f, err := s.policyFile(ctx, sink, req)
		if err != nil {
			return nil, err
		}
		slot := req.Slot
		if req.Entry != nil {
			slot = req.Entry.Slot
		}
		c, err := sess.CheckPolicy(ctx, req.Folder, slot, f)
		if err != nil {
			return nil, err
		}
		return PolicyChecked{PolicyCheck: c, File: f}, nil
	})
}

// InstallPolicy puts a policy into an app's slot (see core.Session.InstallPolicy).
func (s *Service) InstallPolicy(req PolicyRequest) string {
	return s.run("policy", req.Board, req.Folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		f, err := s.policyFile(ctx, sink, req)
		if err != nil {
			return nil, err
		}
		opt := core.InstallPolicyOptions{Slot: req.Slot, File: f, Force: req.Force}
		if e := req.Entry; e != nil {
			opt.Slot = e.Slot
			opt.Meta = core.InstalledPolicy{ID: e.ID, Name: e.Name, Author: e.Author, Index: e.Index}
		} else {
			opt.Meta = core.InstalledPolicy{Name: strings.TrimSpace(req.Name), Author: strings.TrimSpace(req.Author), Source: filepath.Base(f)}
			if opt.Meta.Name == "" {
				opt.Meta.Name = strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
			}
		}
		return nil, sess.InstallPolicy(ctx, sink, req.Folder, opt)
	})
}

// RestorePolicies puts the stock models back (all slots when slots is empty).
func (s *Service) RestorePolicies(boardName, folder string, slots []string, force bool) string {
	return s.run("policyrestore", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.RestorePolicies(ctx, sink, folder, slots, force)
	})
}

// PickPolicyFile asks for an ONNX model.
func (s *Service) PickPolicyFile() (string, error) {
	return s.app.Dialog.OpenFile().SetTitle("Choose a policy (ONNX model)").
		AddFilter("ONNX model (*.onnx)", "*.onnx").PromptForSingleSelection()
}

// SetPolicyIndexes saves the user's own policy indexes (the profiles' community indexes are always read).
func (s *Service) SetPolicyIndexes(urls []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.PolicyIndexes = nil
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			cfg.PolicyIndexes = append(cfg.PolicyIndexes, u)
		}
	}
	return cfg.Save()
}
