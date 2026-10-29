package project

import (
	"testing"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/oidc"
)

func TestAllows(t *testing.T) {
	p := &Project{Allow: []Rule{
		{"iss": GitHubIssuer, "repository": "me/app", "ref": "refs/heads/main"},
		{"iss": GitLabIssuer, "project_path": "me/app", "ref": "main", "ref_protected": "true"},
	}}
	cases := []struct {
		claims oidc.Claims
		want   bool
	}{
		{oidc.Claims{"iss": GitHubIssuer, "repository": "me/app", "ref": "refs/heads/main"}, true},
		{oidc.Claims{"iss": GitHubIssuer, "repository": "me/app", "ref": "refs/heads/dev"}, false},
		{oidc.Claims{"iss": GitHubIssuer, "repository": "me/app2", "ref": "refs/heads/main"}, false},
		{oidc.Claims{"iss": GitLabIssuer, "project_path": "me/app", "ref": "main", "ref_protected": true}, true},
		{oidc.Claims{"iss": GitLabIssuer, "project_path": "me/app", "ref": "main", "ref_protected": false}, false},
		{oidc.Claims{"iss": GitLabIssuer, "repository": "me/app", "ref": "refs/heads/main"}, false},
	}
	for i, c := range cases {
		if got := p.Allows(c.claims); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func TestStore(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p := &Project{Name: "anketa", Type: "node", Port: 3100,
		Allow: []Rule{{"iss": GitHubIssuer, "repository": "me/app"}}}
	if err := s.Put(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("anketa")
	if err != nil || got.Memory != "256m" || got.Health != "/healthz" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.Get("../etc/passwd"); err == nil {
		t.Fatal("path traversal in name accepted")
	}
	bad := &Project{Name: "x1", Type: "node", Port: 3100, Allow: []Rule{{"iss": GitHubIssuer}}}
	if err := s.Put(bad); err == nil {
		t.Fatal("rule with only iss accepted")
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
}
