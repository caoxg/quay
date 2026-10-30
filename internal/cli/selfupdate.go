package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/caoxg/quay/internal/selfupdate"
	"github.com/caoxg/quay/internal/version"
)

func selfUpdateCmd() *cobra.Command {
	var check, yes bool
	c := &cobra.Command{
		Use:   "self-update",
		Short: "Update Quay itself to the newest release",
		Long: "Asks GitHub for the newest Quay release. With your OK it downloads it (through this computer's\n" +
			"proxy, as the download relay does), checks that SHA256SUMS is signed by the Quay release key and\n" +
			"that the file matches it, and replaces this Quay: the app, the Windows folder or this binary.\n" +
			"Installed from the .deb, it downloads the .deb for apt instead. --check only reports.",
		Example: "  quay self-update --check\n  quay self-update",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			u := selfupdate.New()
			rel, err := u.Latest(ctx)
			c := &selfupdate.Check{Checked: time.Now(), Current: version.Version, Latest: rel}
			if err != nil {
				c.Error = err.Error()
			}
			_ = c.Save(selfupdate.CheckFile())
			if err != nil {
				return fmt.Errorf("cannot reach GitHub's releases: %w", err)
			}
			if !selfupdate.Newer(rel.Version, version.Version) {
				if version.Version == "dev" {
					fmt.Printf("this is a development build; the newest release is %s\n", rel.Version)
				} else {
					fmt.Printf("Quay %s is the newest release\n", version.Version)
				}
				return nil
			}
			fmt.Printf("%s is out (this is %s)\n", bold("Quay "+rel.Version), version.Version)
			if notes := strings.TrimSpace(rel.Notes); notes != "" {
				for i, l := range strings.Split(notes, "\n") {
					if i == 12 {
						fmt.Println(dim("  …  " + rel.URL))
						break
					}
					fmt.Println(dim("  " + strings.TrimRight(l, "\r")))
				}
			}
			if check {
				fmt.Println("update with: quay self-update")
				return nil
			}
			t, err := u.Where()
			if err != nil {
				return err
			}
			what := map[string]string{selfupdate.KindMacApp: "replace " + t.Path, selfupdate.KindWinZip: "replace Quay in " + t.Path,
				selfupdate.KindDeb: "download the .deb for apt (" + t.Path + " belongs to the package)"}[t.Kind]
			if what == "" {
				what = "replace " + t.Path
			}
			if !yes {
				ans, err := ask(fmt.Sprintf("%s: %s? [y/N] ", rel.Version, what))
				if err != nil {
					return err
				}
				if !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
					return context.Canceled
				}
			}
			res, err := u.Install(ctx, rel, func(done, total int64) {
				if isTTY && total > 0 {
					fmt.Fprintf(os.Stderr, "\r\033[Kdownloading %d%% (%.1f of %.1f MB)", 100*done/total, float64(done)/(1<<20), float64(total)/(1<<20))
				}
			})
			if isTTY {
				fmt.Fprint(os.Stderr, "\r\033[K")
			}
			if err != nil {
				return err
			}
			if res.Manual != "" {
				fmt.Println(green("✓ ") + "downloaded and verified " + res.File + "; install it with:\n  " + res.Manual)
				return nil
			}
			fmt.Println(green("✓ ") + "Quay " + rel.Version + " is installed (signature and checksum verified); it starts with the next run")
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report whether there is a newer release")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return c
}

// updateNotice mentions a newer release after a command, at most from a check a day old (asking
// GitHub for at most 3 s when the last check is older).
func updateNotice(cmd *cobra.Command) {
	if !isTTY || flagJSON || version.Version == "dev" || cmd == nil || cmd.Name() == "self-update" || cmd.Name() == "gui" {
		return
	}
	file := selfupdate.CheckFile()
	c := selfupdate.LoadCheck(file)
	if c == nil || time.Since(c.Checked) > 24*time.Hour {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		rel, err := selfupdate.New().Latest(ctx)
		cancel()
		c = &selfupdate.Check{Checked: time.Now(), Current: version.Version, Latest: rel}
		if err != nil {
			c.Error = err.Error()
		}
		_ = c.Save(file)
	}
	if c.Available() {
		fmt.Fprintln(os.Stderr, dim("Quay "+c.Latest.Version+" is out (this is "+version.Version+"): quay self-update"))
	}
}
