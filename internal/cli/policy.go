package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/caoxg/quay/internal/config"
	"github.com/caoxg/quay/internal/core"
	"github.com/caoxg/quay/internal/onnxinfo"
	"github.com/caoxg/quay/internal/policystore"
	"github.com/caoxg/quay/internal/profile"
)

// policySources is the community indexes the profiles name plus the ones the user added.
func policySources(profiles []*profile.Profile) ([]policystore.Source, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return policystore.Sources(profiles, cfg.PolicyIndexes), nil
}

func loadIndexes(ctx context.Context, profiles []*profile.Profile) (*policystore.Result, error) {
	src, err := policySources(profiles)
	if err != nil {
		return nil, err
	}
	r := (&policystore.Store{Dir: config.PolicyCacheDir()}).Load(ctx, src)
	for _, p := range r.Problems {
		fmt.Fprintln(os.Stderr, yellow("! ")+p)
	}
	return r, nil
}

func policyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "policy",
		Short: "Install policies (ONNX models) other people trained into an app",
		Long: "Apps that declare policy slots (policies: in quay.yaml; the XGO-Duck profile does) can load\n" +
			"models other people trained. Quay reads them from policy indexes, verifies the download, checks\n" +
			"the model's shapes and metadata against the slot, and keeps the choice across app updates.",
	}
	c.AddCommand(policyListCmd(), policySearchCmd(), policyInstallCmd(), policyRestoreCmd(), policyInspectCmd(), policyIndexCmd())
	return c
}

func policyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list APP",
		Short:   "Show what each policy slot of an installed app holds",
		Example: "  quay policy list xgoduck_runtime_arduino",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				ps, err := s.Policies(ctx, a[0])
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(ps)
				}
				for _, sl := range ps.Slots {
					what := dim("stock")
					if sl.Active != nil {
						what = bold(sl.Active.Name)
						if sl.Active.Author != "" {
							what += dim(" by " + sl.Active.Author)
						}
						if sl.Changed {
							what += yellow(" (the file was changed outside Quay)")
						}
					} else if sl.SHA256 == "" {
						what = red("missing")
					}
					fmt.Printf("%-10s %-24s %s\n", sl.ID, dim(sl.File), what)
				}
				return nil
			})
		},
	}
}

func policySearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "search [PROFILE]",
		Short:   "List the policies in the indexes (for one app profile, e.g. xgoduck)",
		Example: "  quay policy search\n  quay policy search xgoduck",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			profiles := profile.Builtins()
			if len(a) == 1 {
				p := profile.ByID(a[0])
				if p == nil {
					return fmt.Errorf("no built-in profile %q (quay profiles)", a[0])
				}
				profiles = []*profile.Profile{p}
			}
			r, err := loadIndexes(ctx, profiles)
			if err != nil {
				return err
			}
			entries := r.Entries
			if len(a) == 1 {
				entries = policystore.ForApp(entries, a[0])
			}
			if flagJSON {
				return printJSON(entries)
			}
			if len(entries) == 0 {
				fmt.Println(dim("no policies found"))
				return nil
			}
			for _, e := range entries {
				fmt.Printf("%s  %s %s\n", bold(e.ID), e.Name, dim(fmt.Sprintf("— %s · %s/%s · %s", e.Author, e.App, e.Slot, humanSize(e.Size))))
				if e.Description != "" {
					fmt.Printf("  %s\n", e.Description)
				}
			}
			return nil
		},
	}
}

func policyInstallCmd() *cobra.Command {
	var slot, name, author string
	var force bool
	c := &cobra.Command{
		Use:   "install APP ID|FILE.onnx",
		Short: "Install a policy from the indexes, or a model file, into an app",
		Long: "Downloads the policy (or reads FILE.onnx), checks it against the slot, uploads it, and restarts\n" +
			"the app if it runs. If the app does not come up with it, the previous model is put back.\n" +
			"A model file goes into the slot named by --slot, or the slot whose file name it has.",
		Example: "  quay policy install xgoduck_runtime_arduino walk-smooth-alice\n" +
			"  quay policy install xgoduck_runtime_arduino ./model_5500.onnx --slot roller --name \"Roller 5500\"",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			app, what := a[0], a[1]
			local := strings.HasSuffix(strings.ToLower(what), ".onnx")
			if local {
				if _, err := os.Stat(what); err != nil {
					return err
				}
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				opt := core.InstallPolicyOptions{Slot: slot, Force: force}
				if local {
					opt.File = what
					opt.Meta = core.InstalledPolicy{Name: name, Author: author, Source: filepath.Base(what)}
					if opt.Meta.Name == "" {
						opt.Meta.Name = strings.TrimSuffix(filepath.Base(what), filepath.Ext(what))
					}
				} else {
					st, err := s.Status(ctx)
					if err != nil {
						return err
					}
					ap, err := st.App(app)
					if err != nil {
						return err
					}
					r, err := loadIndexes(ctx, []*profile.Profile{ap.Profile})
					if err != nil {
						return err
					}
					e, err := policystore.Find(r.Entries, what)
					if err != nil {
						return err
					}
					if ap.Profile == nil || ap.Profile.ID != e.App {
						return fmt.Errorf("%s is a policy for %s, not for %s", e.ID, e.App, ap.Name)
					}
					if slot != "" && slot != e.Slot {
						return fmt.Errorf("%s is made for the %s slot", e.ID, e.Slot)
					}
					f, err := downloadPolicy(ctx, sink, *e)
					if err != nil {
						return err
					}
					opt.Slot, opt.File = e.Slot, f
					opt.Meta = core.InstalledPolicy{ID: e.ID, Name: e.Name, Author: e.Author, Index: e.Index}
				}
				err := s.InstallPolicy(ctx, sink, app, opt)
				var pe *core.PolicyError
				if errors.As(err, &pe) {
					for _, p := range pe.Problems {
						fmt.Fprintln(os.Stderr, red("  ✗ ")+p)
					}
					return errors.New("not installed (--force installs it anyway)")
				}
				return err
			})
		},
	}
	c.Flags().StringVar(&slot, "slot", "", "slot for a model file (default: the slot with the same file name)")
	c.Flags().StringVar(&name, "name", "", "name to show for a model file (default: the file name)")
	c.Flags().StringVar(&author, "author", "", "author to show for a model file")
	c.Flags().BoolVar(&force, "force", false, "install even if the model fails the checks or the app says stopping is unsafe")
	return c
}

func downloadPolicy(ctx context.Context, sink core.Sink, e policystore.Entry) (string, error) {
	text := fmt.Sprintf("Download %s (%s)", e.Name, humanSize(e.Size))
	sink(core.Event{Type: "step", Step: "download", State: "run", Text: text})
	f, err := (&policystore.Store{Dir: config.PolicyCacheDir()}).Download(ctx, e, nil)
	if err != nil {
		sink(core.Event{Type: "step", Step: "download", State: "fail", Text: err.Error()})
		return "", err
	}
	sink(core.Event{Type: "step", Step: "download", State: "ok", Text: text + ", checksum verified"})
	return f, nil
}

func policyRestoreCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:     "restore APP [SLOT...]",
		Short:   "Put the app's stock models back (all slots, or the ones named)",
		Example: "  quay policy restore xgoduck_runtime_arduino\n  quay policy restore xgoduck_runtime_arduino walk",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				return s.RestorePolicies(ctx, sink, a[0], a[1:], force)
			})
		},
	}
	c.Flags().BoolVar(&force, "force", false, "ignore the app's safety probe")
	return c
}

func policyInspectCmd() *cobra.Command {
	var app, url string
	c := &cobra.Command{
		Use:   "inspect FILE.onnx",
		Short: "Show a model's shapes, metadata and checksum, and an index entry to publish it",
		Long: "Prints what Quay checks in a model, and an entry for a policy index: put the file somewhere\n" +
			"stable (e.g. a GitHub release, plus the same file committed to the repository, which jsDelivr\n" +
			"mirrors), fill in the name, description and license, and send it to the index the app's profile\n" +
			"reads (e.g. for xgoduck, a pull request to github.com/caoxg/awesome-xgo-duck).",
		Example: "  quay policy inspect logs/model_5500.onnx --app xgoduck\n" +
			"  quay policy inspect model.onnx --app xgoduck --url https://github.com/you/repo/releases/download/v1/model.onnx",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			m, err := onnxinfo.ReadFile(a[0])
			if err != nil {
				return err
			}
			sum, size, err := policystore.HashFile(a[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"model": m, "sha256": sum, "size": size})
			}
			for _, t := range m.Inputs {
				fmt.Printf("%s %s %s\n", dim("input "), t.Name, onnxinfo.ShapeString(t.Shape))
			}
			for _, t := range m.Outputs {
				fmt.Printf("%s %s %s\n", dim("output"), t.Name, onnxinfo.ShapeString(t.Shape))
			}
			keys := make([]string, 0, len(m.Metadata))
			for k := range m.Metadata {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("%s %s = %s\n", dim("meta  "), k, m.Metadata[k])
			}
			fmt.Printf("%s %s\n%s %d\n", dim("sha256"), sum, dim("size  "), size)

			slot := "SLOT"
			if p := profile.ByID(app); p != nil && p.Policies != nil {
				base := filepath.Base(a[0])
				for _, sl := range p.Policies.Slots {
					if sl.File == base {
						slot = sl.ID
					}
				}
				var stock *onnxinfo.Model // compared on the board at install time
				probs := []string{}
				for _, sl := range p.Policies.Slots {
					if slot == "SLOT" || sl.ID == slot {
						if pr := policystore.Check(p.Policies, &sl, m, stock); len(pr) > 0 {
							probs = append(probs, fmt.Sprintf("%s: %s", sl.ID, strings.Join(pr, "; ")))
						}
					}
				}
				for _, pr := range probs {
					fmt.Println(yellow("! ") + pr)
				}
				if len(probs) == 0 {
					fmt.Println(green("✓ ") + "fits " + p.Name + " (shapes and required metadata; joint order is compared on the board)")
				}
			} else if app != "" {
				return fmt.Errorf("no built-in profile %q with policy slots", app)
			}
			if url == "" {
				url = "https://github.com/OWNER/REPO/releases/download/TAG/" + filepath.Base(a[0])
			}
			trainer := m.Metadata["task_id"]
			if trainer == "" {
				trainer = "TRAINING CODE AND VERSION"
			}
			if app == "" {
				app = "PROFILE"
			}
			mirrors := ""
			if mu, guessed := policystore.JSDelivr(url); mu != "" {
				mirrors = "    mirrors:\n      - " + mu
				if guessed {
					mirrors += "  # the same file committed to the repository at that tag; fix the path if it isn't at the root"
				}
				mirrors += "\n"
			}
			fmt.Printf("\n%s\n", dim("# index entry"))
			fmt.Printf(`  - id: %s
    app: %s
    slot: %s
    name: NAME
    author: YOU
    description: WHAT IT DOES DIFFERENTLY
    url: %s
%s    sha256: %s
    size: %d
    license: LICENSE
    trainer: %s
    created: %s
`, strings.TrimSuffix(strings.ToLower(filepath.Base(a[0])), ".onnx"), app, slot, url, mirrors, sum, size, trainer, time.Now().Format("2006-01-02"))
			return nil
		},
	}
	c.Flags().StringVar(&app, "app", "", "check against this built-in profile's slots, e.g. xgoduck")
	c.Flags().StringVar(&url, "url", "", "where the file will be published (for the index entry)")
	return c
}

func policyIndexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "index [add URL | rm URL]",
		Short: "List, add or remove policy indexes",
		Long: "Quay always reads the community index each app profile names (`quay profiles`).\n" +
			"Add your own (a URL or a local file) to share policies within a team or a class.",
		Example: "  quay policy index\n  quay policy index add https://example.com/our-policies.yaml\n  quay policy index rm https://example.com/our-policies.yaml",
		Args:    cobra.RangeArgs(0, 2),
		RunE: func(_ *cobra.Command, a []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(a) == 0 {
				for _, s := range policystore.Sources(profile.Builtins(), nil) {
					fmt.Println(s.URL + dim("  (community)"))
				}
				for _, u := range cfg.PolicyIndexes {
					fmt.Println(u)
				}
				return nil
			}
			if len(a) != 2 {
				return errors.New("use `quay policy index add URL` or `quay policy index rm URL`")
			}
			u := a[1]
			switch a[0] {
			case "add":
				if !strings.Contains(u, "://") {
					if abs, err := filepath.Abs(u); err == nil {
						u = abs
					}
				}
				for _, x := range cfg.PolicyIndexes {
					if x == u {
						return nil
					}
				}
				cfg.PolicyIndexes = append(cfg.PolicyIndexes, u)
			case "rm", "remove":
				keep := cfg.PolicyIndexes[:0]
				for _, x := range cfg.PolicyIndexes {
					if x != u {
						keep = append(keep, x)
					}
				}
				cfg.PolicyIndexes = keep
			default:
				return errors.New("use add or rm")
			}
			return cfg.Save()
		},
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "size unknown"
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
