package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePorkbun answers like the API: keys checked on every call, ids as numbers on create
// and as strings on retrieve.
func fakePorkbun(t *testing.T) (*Porkbun, *[]Record) {
	t.Helper()
	recs := &[]Record{{ID: "100", Name: "example.com", Type: "A", Content: "1.2.3.4"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["apikey"] != "pk" || body["secretapikey"] != "sk" {
			json.NewEncoder(w).Encode(map[string]string{"status": "ERROR", "message": "Invalid API key."})
			return
		}
		switch {
		case r.URL.Path == "/dns/retrieve/example.com":
			json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "records": *recs})
		case r.URL.Path == "/dns/create/example.com":
			*recs = append(*recs, Record{ID: "200", Name: body["name"].(string) + ".example.com", Type: "A",
				Content: body["content"].(string), Notes: body["notes"].(string)})
			w.Write([]byte(`{"status":"SUCCESS","id":200}`))
		case strings.HasPrefix(r.URL.Path, "/dns/delete/example.com/"):
			id := strings.TrimPrefix(r.URL.Path, "/dns/delete/example.com/")
			kept := (*recs)[:0]
			for _, rec := range *recs {
				if rec.ID != id {
					kept = append(kept, rec)
				}
			}
			*recs = kept
			w.Write([]byte(`{"status":"SUCCESS"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Porkbun{Keys: Keys{APIKey: "pk", SecretKey: "sk"}, Base: srv.URL}, recs
}

func TestPorkbunRoundTrip(t *testing.T) {
	p, recs := fakePorkbun(t)
	ctx := context.Background()
	id, err := p.CreateA(ctx, "example.com", "app", "1.2.3.4", "cd:app")
	if err != nil || id != "200" {
		t.Fatalf("create: %q %v", id, err)
	}
	rs, err := p.Records(ctx, "example.com")
	if err != nil || len(rs) != 2 || rs[1].ID != "200" || rs[1].Notes != "cd:app" {
		t.Fatalf("records: %+v %v", rs, err)
	}
	if err := p.Delete(ctx, "example.com", "200"); err != nil || len(*recs) != 1 {
		t.Fatalf("delete: %v, left %+v", err, *recs)
	}
	if err := p.Delete(ctx, "example.com", "../100"); err == nil {
		t.Fatal("a non-numeric id must be refused")
	}
	p.Keys.SecretKey = "wrong"
	if _, err := p.Records(ctx, "example.com"); err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("bad keys: %v", err)
	}
}

func TestSub(t *testing.T) {
	ok := map[string]string{"app.example.com": "app", "My-App2.Example.com": "my-app2"}
	for in, want := range ok {
		if got, err := Sub(in, "example.com"); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"example.com", "www.example.com", "a.b.example.com", "app.other.com",
		"-app.example.com", "app.example.com.evil.com", "appexample.com"} {
		if _, err := Sub(bad, "example.com"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestPlan(t *testing.T) {
	rs := []Record{
		{Name: "shop.example.com", Type: "A", Content: "1.2.3.4"},
		{Name: "mail.example.com", Type: "CNAME", Content: "mx.other.com"},
		{Name: "old.example.com", Type: "A", Content: "9.9.9.9"},
	}
	cases := map[string]string{"new.example.com": "create", "shop.example.com": "exists", "SHOP.example.com": "exists"}
	for name, want := range cases {
		if got, err := Plan(rs, name, "1.2.3.4"); err != nil || got != want {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	for _, name := range []string{"mail.example.com", "old.example.com"} {
		if _, err := Plan(rs, name, "1.2.3.4"); err == nil {
			t.Errorf("%s: an existing foreign record must stop the plan", name)
		}
	}
}
