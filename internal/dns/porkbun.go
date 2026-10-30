// Package dns keeps the A records of cd's projects in the owner's DNS zone, through the
// Porkbun API. It creates a record only for a name that has no records at all, marks it as
// cd's, and deletes only records it created (by id): the rest of the zone is never touched.
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const DefaultBase = "https://api.porkbun.com/api/json/v3"

// Keys is the owner's API key pair, kept in a root-only file on the server.
type Keys struct {
	APIKey    string `json:"apikey"`
	SecretKey string `json:"secretapikey"`
}

func LoadKeys(path string) (Keys, error) {
	var k Keys
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, fmt.Errorf("%s: %w", path, err)
	}
	if k.APIKey == "" || k.SecretKey == "" {
		return k, fmt.Errorf("%s: apikey and secretapikey are required", path)
	}
	return k, nil
}

func SaveKeys(path string, k Keys) error {
	b, _ := json.MarshalIndent(k, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type Record struct {
	ID      string `json:"id"`
	Name    string `json:"name"` // the full name, e.g. app.example.com
	Type    string `json:"type"`
	Content string `json:"content"`
	Notes   string `json:"notes"`
}

type Porkbun struct {
	Keys   Keys
	Base   string // DefaultBase when empty
	Client *http.Client
}

func (p *Porkbun) call(ctx context.Context, path string, body map[string]any, out any) error {
	if body == nil {
		body = map[string]any{}
	}
	body["apikey"], body["secretapikey"] = p.Keys.APIKey, p.Keys.SecretKey
	b, _ := json.Marshal(body)
	base := p.Base
	if base == "" {
		base = DefaultBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := p.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("porkbun: %w", err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return fmt.Errorf("porkbun: HTTP %d, unreadable answer", resp.StatusCode)
	}
	var st struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &st)
	if st.Status != "SUCCESS" {
		msg := st.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("porkbun: %s", msg)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Records lists every record of the zone.
func (p *Porkbun) Records(ctx context.Context, zone string) ([]Record, error) {
	var out struct {
		Records []struct {
			Record
			ID json.RawMessage `json:"id"`
		} `json:"records"`
	}
	if err := p.call(ctx, "/dns/retrieve/"+zone, nil, &out); err != nil {
		return nil, err
	}
	rs := make([]Record, 0, len(out.Records))
	for _, r := range out.Records {
		r.Record.ID = idString(r.ID)
		rs = append(rs, r.Record)
	}
	return rs, nil
}

// CreateA adds an A record for sub.zone and returns its id.
func (p *Porkbun) CreateA(ctx context.Context, zone, sub, ip, notes string) (string, error) {
	var out struct {
		ID json.RawMessage `json:"id"`
	}
	err := p.call(ctx, "/dns/create/"+zone, map[string]any{
		"name": sub, "type": "A", "content": ip, "ttl": "600", "notes": notes,
	}, &out)
	if err != nil {
		return "", err
	}
	id := idString(out.ID)
	if id == "" {
		return "", errors.New("porkbun: created, but the answer has no record id")
	}
	return id, nil
}

// Delete removes one record by id.
func (p *Porkbun) Delete(ctx context.Context, zone, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("bad record id %q", id)
	}
	return p.call(ctx, "/dns/delete/"+zone+"/"+id, nil, nil)
}

var idRe = regexp.MustCompile(`^[0-9]+$`)

// idString accepts the id as a JSON number or string.
func idString(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Sub returns the single label of domain under zone ("app" for app.example.com), or an error
// for anything else: the zone itself, www, deeper names, other zones.
func Sub(domain, zone string) (string, error) {
	sub, ok := strings.CutSuffix(strings.ToLower(domain), "."+strings.ToLower(zone))
	if !ok || !labelRe.MatchString(sub) {
		return "", fmt.Errorf("%s is not a name directly under %s (like app.%s)", domain, zone, zone)
	}
	if sub == "www" {
		return "", fmt.Errorf("www.%s is reserved", zone)
	}
	return sub, nil
}

// Plan says what to do with fqdn given the zone's records:
//
//	create — no record of any type has this name: create an A record to ip;
//	exists — it already has A records, all to ip (made by hand): use it, never delete it;
//	error  — anything else (another address, a CNAME, …): leave the zone alone.
func Plan(records []Record, fqdn, ip string) (action string, err error) {
	var same []Record
	for _, r := range records {
		if !strings.EqualFold(r.Name, fqdn) {
			continue
		}
		if r.Type != "A" || r.Content != ip {
			return "", fmt.Errorf("%s already has a %s record to %s: not touching it", fqdn, r.Type, r.Content)
		}
		same = append(same, r)
	}
	if len(same) > 0 {
		return "exists", nil
	}
	return "create", nil
}
