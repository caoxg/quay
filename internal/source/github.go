package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/version"
)

// Commit describes a commit, as far as the host's API tells.
type Commit struct {
	Hash    string    `json:"hash"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	// Ahead is how many commits it is ahead of the base asked about (-1 when not compared).
	Ahead int `json:"ahead"`
}

var githubRepo = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)

// DescribeCommit looks a commit up on GitHub (subject, date) and, with base set, counts the
// commits since base. Other hosts are not supported: it returns an error, and callers fall back
// to the bare hash.
func DescribeCommit(ctx context.Context, repoURL, hash, base string) (*Commit, error) {
	m := githubRepo.FindStringSubmatch(NormalizeURL(repoURL))
	if m == nil {
		return nil, fmt.Errorf("%s: commit details are only looked up on GitHub", repoURL)
	}
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s", m[1], m[2])
	type commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	out := &Commit{Hash: hash, Ahead: -1}
	fill := func(c commit) {
		out.Subject, _, _ = strings.Cut(c.Commit.Message, "\n")
		out.Date = c.Commit.Committer.Date
	}
	if base != "" && base != hash {
		var cmp struct {
			AheadBy int      `json:"ahead_by"`
			Status  string   `json:"status"`
			Commits []commit `json:"commits"`
		}
		if err := githubGet(ctx, api+"/compare/"+base+"..."+hash, &cmp); err == nil {
			out.Ahead = cmp.AheadBy
			for _, c := range cmp.Commits {
				if c.SHA == hash {
					fill(c)
					return out, nil
				}
			}
		}
	}
	var c commit
	if err := githubGet(ctx, api+"/commits/"+hash, &c); err != nil {
		return nil, err
	}
	fill(c)
	if base == hash {
		out.Ahead = 0
	}
	return out, nil
}

func githubGet(ctx context.Context, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Quay/"+version.Version)
	if tok := os.Getenv("QUAY_GIT_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
