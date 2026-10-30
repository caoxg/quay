package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/a/b/compare/old...new": // forward: two commits, oldest first
			w.Write([]byte(`{"ahead_by":2,"behind_by":0,"merge_base_commit":{"sha":"old"},"commits":[
				{"sha":"mid","commit":{"message":"Fix the servo scan\n\nlonger text","author":{"name":"Ann"},"committer":{"date":"2026-10-28T10:00:00Z"}}},
				{"sha":"new","commit":{"message":"Smooth actions by default","author":{"name":"Bo"},"committer":{"date":"2026-10-29T10:00:00Z"}}}]}`))
		case "/repos/a/b/compare/new...old": // backward: head is an ancestor of base
			w.Write([]byte(`{"ahead_by":0,"behind_by":2,"merge_base_commit":{"sha":"old","commit":{"message":"Release 1.0","author":{"name":"Ann"},"committer":{"date":"2026-10-20T10:00:00Z"}}},"commits":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer func(s string) { githubAPI = s }(githubAPI)
	githubAPI = srv.URL

	c, err := Compare(context.Background(), "a/b", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if c.Ahead != 2 || c.Behind != 0 || len(c.Commits) != 2 || c.Commits[0].Hash != "new" || c.Commits[1].Subject != "Fix the servo scan" || c.Commits[1].Author != "Ann" {
		t.Fatalf("forward: %+v", c)
	}
	if c.Head.Hash != "new" || c.Head.Subject != "Smooth actions by default" {
		t.Fatalf("forward head: %+v", c.Head)
	}
	c, err = Compare(context.Background(), "https://github.com/a/b.git", "new", "old")
	if err != nil {
		t.Fatal(err)
	}
	if c.Ahead != 0 || c.Behind != 2 || len(c.Commits) != 0 || c.Head.Subject != "Release 1.0" {
		t.Fatalf("backward: %+v", c)
	}
	if _, err := Compare(context.Background(), "a/b", "old", "gone"); err == nil {
		t.Fatal("an unknown commit must be an error")
	}
	if _, err := Compare(context.Background(), "https://gitlab.com/a/b.git", "old", "new"); err == nil {
		t.Fatal("hosts other than GitHub are not asked")
	}
}
