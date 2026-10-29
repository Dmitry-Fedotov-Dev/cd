// Package server is cdd's HTTP API: an OIDC token from the CI job decides which project the
// caller may touch; the answer is a streamed log that ends with "CD-RESULT: <code> <message>".
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/engine"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/oidc"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
)

// Denied is the result code for a token that is missing, invalid or not allowed for the project.
const Denied = 5

type Verifier interface {
	Verify(ctx context.Context, raw string) (oidc.Claims, error)
}

type Engine interface {
	Deploy(ctx context.Context, p *project.Project, sha, by string, src io.Reader, out io.Writer) engine.Result
	Status(ctx context.Context, p *project.Project, out io.Writer) engine.Result
	Logs(ctx context.Context, p *project.Project, n int, out io.Writer) engine.Result
	Rollback(ctx context.Context, p *project.Project, out io.Writer) engine.Result
}

type Server struct {
	Store    project.Store
	Verifier Verifier
	Engine   Engine
	Log      *slog.Logger
	Version  string
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": s.Version})
	})
	mux.HandleFunc("POST /v1/{action}", s.action)
	return mux
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	name := r.URL.Query().Get("project")
	log := s.Log.With("action", action, "project", name, "remote", r.Header.Get("X-Forwarded-For"))
	finish := func(code int, status int, msg string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "CD-RESULT: %d %s\n", code, msg)
		log.Info("request", "result", code, "msg", msg)
	}

	switch action {
	case "deploy", "status", "logs", "rollback":
	default:
		finish(engine.BadInput, http.StatusNotFound, "unknown action "+action)
		return
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		finish(Denied, http.StatusUnauthorized, "no token: the job needs an OIDC token for this server (GitHub: permissions id-token: write)")
		return
	}
	claims, err := s.Verifier.Verify(r.Context(), strings.TrimSpace(raw))
	if err != nil {
		finish(Denied, http.StatusUnauthorized, "token rejected: "+err.Error())
		return
	}
	who := identity(claims)
	log = log.With("by", who)
	p, err := s.Store.Get(name)
	if err != nil {
		// the same answer for "no such project" and "not yours": do not reveal project names
		log.Info("project lookup", "err", err)
		finish(Denied, http.StatusForbidden, "project "+name+" does not exist or this repository/branch may not deploy it")
		return
	}
	if !p.Allows(claims) {
		finish(Denied, http.StatusForbidden, "project "+name+" does not exist or this repository/branch may not deploy it")
		return
	}

	sha := r.URL.Query().Get("sha")
	if action == "deploy" && !shaRe.MatchString(sha) {
		finish(engine.BadInput, http.StatusBadRequest, "sha must be 7-64 hex characters")
		return
	}
	if action == "deploy" && r.ContentLength > int64(p.MaxBundleMB)<<20 {
		finish(engine.Rejected, http.StatusRequestEntityTooLarge, fmt.Sprintf("bundle over %d MB", p.MaxBundleMB))
		return
	}

	// Take the whole bundle before touching anything: a cut upload never starts a deploy.
	var bundleFile *os.File
	if action == "deploy" {
		f, err := os.CreateTemp("", "cd-bundle-*.tgz")
		if err != nil {
			finish(engine.Failed, http.StatusInternalServerError, "no temp space")
			return
		}
		defer os.Remove(f.Name())
		defer f.Close()
		limit := int64(p.MaxBundleMB) << 20
		n, err := io.Copy(f, io.LimitReader(r.Body, limit+1))
		switch {
		case err != nil:
			finish(engine.Rejected, http.StatusBadRequest, "upload interrupted")
			return
		case n > limit:
			finish(engine.Rejected, http.StatusRequestEntityTooLarge, fmt.Sprintf("bundle over %d MB", p.MaxBundleMB))
			return
		case n == 0:
			finish(engine.Rejected, http.StatusBadRequest, "empty bundle")
			return
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			finish(engine.Failed, http.StatusInternalServerError, err.Error())
			return
		}
		bundleFile = f
	}

	// From here the answer is a stream; the work must not stop if the client goes away mid-swap.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	out := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		out.f = f
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()

	start := time.Now()
	var res engine.Result
	switch action {
	case "deploy":
		fmt.Fprintf(out, "cd %s: deploy %s to %s (by %s)\n", s.Version, short(sha), p.Name, who)
		res = s.Engine.Deploy(ctx, p, sha, who, bundleFile, out)
	case "status":
		res = s.Engine.Status(ctx, p, out)
	case "logs":
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		res = s.Engine.Logs(ctx, p, min(max(n, 1), 1000), out)
	case "rollback":
		res = s.Engine.Rollback(ctx, p, out)
	}
	fmt.Fprintf(out, "CD-RESULT: %d %s\n", res.Code, res.Msg)
	log.Info("request", "sha", sha, "result", res.Code, "msg", res.Msg, "took", time.Since(start).Round(time.Second).String())
}

// identity is a short, human-readable "who" for logs and the release record.
func identity(c oidc.Claims) string {
	if repo := c.String("repository"); repo != "" { // GitHub
		return fmt.Sprintf("%s@%s (%s, run %s)", repo, strings.TrimPrefix(c.String("ref"), "refs/heads/"), c.String("actor"), c.String("run_id"))
	}
	if path := c.String("project_path"); path != "" { // GitLab
		return fmt.Sprintf("%s@%s (%s, pipeline %s)", path, c.String("ref"), c.String("user_login"), c.String("pipeline_id"))
	}
	return c.String("sub")
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
