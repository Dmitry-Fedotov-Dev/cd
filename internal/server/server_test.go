package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/engine"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/oidc"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
)

// fakeVerifier accepts tokens of the form "repo:ref".
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, raw string) (oidc.Claims, error) {
	repo, ref, ok := strings.Cut(raw, ":")
	if !ok {
		return nil, errors.New("bad token")
	}
	return oidc.Claims{"iss": project.GitHubIssuer, "repository": repo, "ref": ref}, nil
}

type fakeEngine struct{ deployed string }

func (f *fakeEngine) Deploy(_ context.Context, p *project.Project, sha, _ string, src io.Reader, out io.Writer) engine.Result {
	b, _ := io.ReadAll(src)
	f.deployed = p.Name + "@" + sha + ":" + string(b)
	io.WriteString(out, "building\n")
	return engine.Result{Code: engine.OK, Msg: "deployed"}
}
func (f *fakeEngine) Status(context.Context, *project.Project, io.Writer) engine.Result {
	return engine.Result{Code: engine.OK, Msg: "healthy"}
}
func (f *fakeEngine) Logs(context.Context, *project.Project, int, io.Writer) engine.Result {
	return engine.Result{}
}
func (f *fakeEngine) Rollback(context.Context, *project.Project, io.Writer) engine.Result {
	return engine.Result{}
}

func setup(t *testing.T) (*httptest.Server, *fakeEngine) {
	t.Helper()
	store := project.Store{Dir: t.TempDir()}
	if err := store.Put(&project.Project{Name: "app", Type: "node", Port: 3100,
		Allow: []project.Rule{{"iss": project.GitHubIssuer, "repository": "me/app", "ref": "refs/heads/main"}}}); err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{}
	s := &Server{Store: store, Verifier: fakeVerifier{}, Engine: eng, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, eng
}

func call(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDeployAllowed(t *testing.T) {
	srv, eng := setup(t)
	code, body := call(t, srv.URL+"/v1/deploy?project=app&sha=abcdef1", "me/app:refs/heads/main", "BUNDLE")
	if code != 200 || !strings.HasSuffix(body, "CD-RESULT: 0 deployed\n") || !strings.Contains(body, "building") {
		t.Fatalf("%d %q", code, body)
	}
	if eng.deployed != "app@abcdef1:BUNDLE" {
		t.Fatalf("engine got %q", eng.deployed)
	}
}

func TestDenied(t *testing.T) {
	srv, eng := setup(t)
	cases := map[string]struct{ url, token string }{
		"no token":      {"/v1/deploy?project=app&sha=abcdef1", ""},
		"bad token":     {"/v1/deploy?project=app&sha=abcdef1", "garbage"},
		"other repo":    {"/v1/deploy?project=app&sha=abcdef1", "evil/app:refs/heads/main"},
		"other branch":  {"/v1/deploy?project=app&sha=abcdef1", "me/app:refs/heads/dev"},
		"other project": {"/v1/deploy?project=nope&sha=abcdef1", "me/app:refs/heads/main"},
		"traversal":     {"/v1/deploy?project=../app&sha=abcdef1", "me/app:refs/heads/main"},
	}
	for name, c := range cases {
		code, body := call(t, srv.URL+c.url, c.token, "x")
		if code < 400 || !strings.HasPrefix(body, "CD-RESULT: 5 ") {
			t.Errorf("%s: %d %q", name, code, body)
		}
	}
	if eng.deployed != "" {
		t.Fatal("engine was called")
	}
	if code, body := call(t, srv.URL+"/v1/deploy?project=app&sha=nothex", "me/app:refs/heads/main", "x"); code != 400 || !strings.HasPrefix(body, "CD-RESULT: 4 ") {
		t.Errorf("bad sha: %d %q", code, body)
	}
	if code, _ := call(t, srv.URL+"/v1/shell?project=app", "me/app:refs/heads/main", ""); code != 404 {
		t.Errorf("unknown action: %d", code)
	}
}

func TestStatus(t *testing.T) {
	srv, _ := setup(t)
	if _, body := call(t, srv.URL+"/v1/status?project=app", "me/app:refs/heads/main", ""); !strings.Contains(body, "CD-RESULT: 0 healthy") {
		t.Fatal(body)
	}
}
