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

// Change is one commit in a comparison.
type Change struct {
	Hash    string    `json:"hash"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

// Comparison is what moving from base to head changes.
type Comparison struct {
	Head   Change `json:"head"`   // the commit compared to
	Ahead  int    `json:"ahead"`  // commits head has that base lacks
	Behind int    `json:"behind"` // commits base has that head lacks: moving there takes them away
	// Commits are the ahead ones, newest first. GitHub lists at most 250.
	Commits []Change `json:"commits"`
}

var githubRepo = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)

// githubAPI is where GitHub's REST API is (a test server in tests).
var githubAPI = "https://api.github.com"

type ghCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (c ghCommit) change() Change {
	subject, _, _ := strings.Cut(c.Commit.Message, "\n")
	return Change{Hash: c.SHA, Subject: subject, Author: c.Commit.Author.Name, Date: c.Commit.Committer.Date}
}

type ghComparison struct {
	AheadBy         int        `json:"ahead_by"`
	BehindBy        int        `json:"behind_by"`
	MergeBaseCommit ghCommit   `json:"merge_base_commit"`
	Commits         []ghCommit `json:"commits"` // oldest first
}

func repoAPI(repoURL string) (string, error) {
	m := githubRepo.FindStringSubmatch(NormalizeURL(repoURL))
	if m == nil {
		return "", fmt.Errorf("%s: commit details are only looked up on GitHub", repoURL)
	}
	return fmt.Sprintf("%s/repos/%s/%s", githubAPI, m[1], m[2]), nil
}

// DescribeCommit looks a commit up on GitHub (subject, date) and, with base set, counts the
// commits since base. Other hosts are not supported: it returns an error, and callers fall back
// to the bare hash.
func DescribeCommit(ctx context.Context, repoURL, hash, base string) (*Commit, error) {
	api, err := repoAPI(repoURL)
	if err != nil {
		return nil, err
	}
	out := &Commit{Hash: hash, Ahead: -1}
	fill := func(c ghCommit) {
		ch := c.change()
		out.Subject, out.Date = ch.Subject, ch.Date
	}
	if base != "" && base != hash {
		var cmp ghComparison
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
	var c ghCommit
	if err := githubGet(ctx, api+"/commits/"+hash, &c); err != nil {
		return nil, err
	}
	fill(c)
	if base == hash {
		out.Ahead = 0
	}
	return out, nil
}

// Compare asks GitHub what moving from base to head changes: the commits head adds (newest
// first) and how many of base's it takes away. Other hosts are not supported (an error).
func Compare(ctx context.Context, repoURL, base, head string) (*Comparison, error) {
	api, err := repoAPI(repoURL)
	if err != nil {
		return nil, err
	}
	var cmp ghComparison
	if err := githubGet(ctx, api+"/compare/"+base+"..."+head, &cmp); err != nil {
		return nil, err
	}
	out := &Comparison{Ahead: cmp.AheadBy, Behind: cmp.BehindBy, Commits: make([]Change, 0, len(cmp.Commits))}
	for i := len(cmp.Commits) - 1; i >= 0; i-- {
		out.Commits = append(out.Commits, cmp.Commits[i].change())
	}
	switch {
	case len(out.Commits) > 0 && strings.HasPrefix(out.Commits[0].Hash, head):
		out.Head = out.Commits[0]
	case strings.HasPrefix(cmp.MergeBaseCommit.SHA, head): // head is an ancestor of base
		out.Head = cmp.MergeBaseCommit.change()
	default:
		out.Head = Change{Hash: head}
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
