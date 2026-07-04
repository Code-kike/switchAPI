package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Code-kike/switchAPI/internal/hub/pricing"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
)

type stubChannel struct {
	mu         sync.Mutex
	broadcasts int
	kicked     []string
}

func (s *stubChannel) Broadcast() { s.mu.Lock(); s.broadcasts++; s.mu.Unlock() }
func (s *stubChannel) Kick(id string) {
	s.mu.Lock()
	s.kicked = append(s.kicked, id)
	s.mu.Unlock()
}
func (s *stubChannel) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.broadcasts }

type testRig struct {
	st     *store.Store
	key    []byte
	ch     *stubChannel
	pricer *pricing.Resolver
	srv    *httptest.Server
	auth   *http.Client // logged-in client (cookie jar)
	anon   *http.Client
}

// reloadPricer refreshes the resolver after seeding prices/overrides mid-test.
func (r *testRig) reloadPricer(t *testing.T) {
	t.Helper()
	if err := r.pricer.Reload(); err != nil {
		t.Fatal(err)
	}
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, err := cryptoutil.LoadOrCreateMasterKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ch := &stubChannel{}
	resolver, err := pricing.NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, key, ch, resolver).Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	rig := &testRig{st: st, key: key, ch: ch, pricer: resolver, srv: srv,
		auth: &http.Client{Jar: jar}, anon: &http.Client{}}

	// bootstrap login sets the admin password
	if code, _ := rig.do(rig.auth, "POST", "/api/v1/auth/login", `{"password":"pw-测试"}`); code != 200 {
		t.Fatalf("bootstrap login = %d", code)
	}
	return rig
}

func (r *testRig) do(cli *http.Client, method, path, body string) (int, []byte) {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cli.Do(req)
	if err != nil {
		return -1, nil
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (r *testRig) createProvider(t *testing.T, name, protocol, key string) string {
	t.Helper()
	code, body := r.do(r.auth, "POST", "/api/v1/providers", fmt.Sprintf(
		`{"name":%q,"protocol":%q,"base_url":"https://relay.example/","api_key":%q}`,
		name, protocol, key))
	if code != 201 {
		t.Fatalf("create provider = %d: %s", code, body)
	}
	var v struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &v)
	return v.ID
}

func TestAuthMiddlewareAndBootstrap(t *testing.T) {
	r := newTestRig(t)

	// Anonymous requests: protected route 401, healthz open.
	if code, _ := r.do(r.anon, "GET", "/api/v1/providers", ""); code != 401 {
		t.Fatalf("unauth providers = %d, want 401", code)
	}
	if code, _ := r.do(r.anon, "GET", "/healthz", ""); code != 200 {
		t.Fatalf("healthz = %d", code)
	}

	// Password was set by bootstrap: wrong password rejected, correct accepted.
	if code, _ := r.do(r.anon, "POST", "/api/v1/auth/login", `{"password":"wrong"}`); code != 401 {
		t.Fatalf("wrong password = %d, want 401", code)
	}
	jar, _ := cookiejar.New(nil)
	second := &http.Client{Jar: jar}
	if code, _ := r.do(second, "POST", "/api/v1/auth/login", `{"password":"pw-测试"}`); code != 200 {
		t.Fatalf("correct password rejected")
	}
	if code, _ := r.do(second, "GET", "/api/v1/providers", ""); code != 200 {
		t.Fatalf("logged-in providers = %d", code)
	}

	// Logout invalidates the session.
	if code, _ := r.do(second, "POST", "/api/v1/auth/logout", `{}`); code != 200 {
		t.Fatal("logout failed")
	}
	if code, _ := r.do(second, "GET", "/api/v1/providers", ""); code != 401 {
		t.Fatal("session survived logout")
	}
}

func TestProviderCRUDMaskingAndEncryptionAtRest(t *testing.T) {
	r := newTestRig(t)
	const plainKey = "sk-live-abcd1234"
	id := r.createProvider(t, "站点A", "anthropic", plainKey)

	// Response masks the key to last4; raw body never contains the plaintext.
	code, body := r.do(r.auth, "GET", "/api/v1/providers", "")
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if bytes.Contains(body, []byte(plainKey)) {
		t.Fatal("plaintext api key leaked in list response")
	}
	if !bytes.Contains(body, []byte(`"key_last4":"1234"`)) {
		t.Fatalf("key_last4 missing: %s", body)
	}
	// base_url trailing slash trimmed
	if !bytes.Contains(body, []byte(`"base_url":"https://relay.example"`)) {
		t.Fatalf("base_url not normalized: %s", body)
	}

	// At rest: ciphertext only.
	p, err := r.st.GetProvider(id)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(p.APIKeyEnc, []byte(plainKey)) {
		t.Fatal("api key stored in plaintext")
	}
	if plain, err := cryptoutil.Open(r.key, p.APIKeyEnc); err != nil || string(plain) != plainKey {
		t.Fatalf("stored ciphertext does not decrypt to original: %v", err)
	}

	// Update without api_key keeps the old one.
	if code, _ := r.do(r.auth, "PUT", "/api/v1/providers/"+id, `{"name":"改名"}`); code != 200 {
		t.Fatalf("update = %d", code)
	}
	code, body = r.do(r.auth, "GET", "/api/v1/providers", "")
	if !bytes.Contains(body, []byte(`"key_last4":"1234"`)) || !bytes.Contains(body, []byte("改名")) {
		t.Fatalf("update lost key or name: %d %s", code, body)
	}
	// Update with a new key replaces it.
	if code, _ := r.do(r.auth, "PUT", "/api/v1/providers/"+id, `{"api_key":"sk-new-key-9999"}`); code != 200 {
		t.Fatal("key update failed")
	}
	_, body = r.do(r.auth, "GET", "/api/v1/providers", "")
	if !bytes.Contains(body, []byte(`"key_last4":"9999"`)) {
		t.Fatalf("key not replaced: %s", body)
	}
	// Protocol is immutable.
	if code, _ := r.do(r.auth, "PUT", "/api/v1/providers/"+id, `{"protocol":"openai"}`); code != 400 {
		t.Fatal("protocol change accepted")
	}

	if code, _ := r.do(r.auth, "DELETE", "/api/v1/providers/"+id, ""); code != 200 {
		t.Fatal("delete failed")
	}
	if code, _ := r.do(r.auth, "DELETE", "/api/v1/providers/"+id, ""); code != 404 {
		t.Fatal("double delete not 404")
	}
}

func TestSwitchFlow(t *testing.T) {
	r := newTestRig(t)
	p1 := r.createProvider(t, "A1", "anthropic", "sk-a1-0001")
	p2 := r.createProvider(t, "A2", "anthropic", "sk-a2-0002")
	p3 := r.createProvider(t, "O1", "openai", "sk-o1-0003")

	// Happy path + broadcast.
	before := r.ch.count()
	if code, body := r.do(r.auth, "POST", "/api/v1/switch",
		`{"app":"claude-code","provider_id":"`+p1+`"}`); code != 200 {
		t.Fatalf("switch = %d: %s", code, body)
	}
	if r.ch.count() != before+1 {
		t.Fatal("switch did not broadcast")
	}

	// Validation: protocol mismatch / ghost provider / bad app.
	if code, _ := r.do(r.auth, "POST", "/api/v1/switch",
		`{"app":"claude-code","provider_id":"`+p3+`"}`); code != 400 {
		t.Fatal("protocol mismatch accepted")
	}
	if code, _ := r.do(r.auth, "POST", "/api/v1/switch",
		`{"app":"claude-code","provider_id":"ghost"}`); code != 404 {
		t.Fatal("ghost provider accepted")
	}
	if code, _ := r.do(r.auth, "POST", "/api/v1/switch",
		`{"app":"vscode","provider_id":"`+p1+`"}`); code != 400 {
		t.Fatal("unknown app accepted")
	}

	// State reflects the switch.
	_, body := r.do(r.auth, "GET", "/api/v1/state", "")
	if !bytes.Contains(body, []byte(p1)) {
		t.Fatalf("state missing active provider: %s", body)
	}

	// Active provider cannot be deleted (409), switch away then delete works.
	if code, _ := r.do(r.auth, "DELETE", "/api/v1/providers/"+p1, ""); code != 409 {
		t.Fatal("active provider deletable")
	}
	if code, _ := r.do(r.auth, "POST", "/api/v1/switch",
		`{"app":"claude-code","provider_id":"`+p2+`"}`); code != 200 {
		t.Fatal("second switch failed")
	}
	if code, _ := r.do(r.auth, "DELETE", "/api/v1/providers/"+p1, ""); code != 200 {
		t.Fatal("delete after switch-away failed")
	}

	// Timeline has switch events.
	_, body = r.do(r.auth, "GET", "/api/v1/events?limit=10", "")
	if !bytes.Contains(body, []byte(`"kind":"switch"`)) {
		t.Fatalf("no switch event: %s", body)
	}
}

func TestFallbackOrder(t *testing.T) {
	r := newTestRig(t)
	p1 := r.createProvider(t, "A1", "anthropic", "sk-a1-0001")
	p2 := r.createProvider(t, "A2", "anthropic", "sk-a2-0002")
	o1 := r.createProvider(t, "O1", "openai", "sk-o1-0003")

	if code, body := r.do(r.auth, "PUT", "/api/v1/fallback-order/claude-code",
		`{"provider_ids":["`+p2+`","`+p1+`"]}`); code != 200 {
		t.Fatalf("put fallback = %d: %s", code, body)
	}
	_, body := r.do(r.auth, "GET", "/api/v1/fallback-order/claude-code", "")
	var got struct {
		ProviderIDs []string `json:"provider_ids"`
	}
	json.Unmarshal(body, &got)
	if len(got.ProviderIDs) != 2 || got.ProviderIDs[0] != p2 {
		t.Fatalf("fallback order = %v", got.ProviderIDs)
	}

	if code, _ := r.do(r.auth, "PUT", "/api/v1/fallback-order/claude-code",
		`{"provider_ids":["ghost"]}`); code != 400 {
		t.Fatal("ghost in fallback accepted")
	}
	if code, _ := r.do(r.auth, "PUT", "/api/v1/fallback-order/claude-code",
		`{"provider_ids":["`+o1+`"]}`); code != 400 {
		t.Fatal("wrong-protocol fallback accepted")
	}
}

func TestPairingFlow(t *testing.T) {
	r := newTestRig(t)

	code, body := r.do(r.auth, "POST", "/api/v1/devices/pairing-code", `{}`)
	if code != 200 {
		t.Fatalf("pairing-code = %d", code)
	}
	var pc struct {
		Code string `json:"code"`
	}
	json.Unmarshal(body, &pc)
	if len(pc.Code) != 6 {
		t.Fatalf("code = %q", pc.Code)
	}

	// Pair WITHOUT a session (code-gated public endpoint).
	code, body = r.do(r.anon, "POST", "/api/v1/devices/pair",
		`{"code":"`+pc.Code+`","name":"dev1","platform":"linux"}`)
	if code != 200 {
		t.Fatalf("pair = %d: %s", code, body)
	}
	var paired struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	json.Unmarshal(body, &paired)
	if paired.DeviceID == "" || len(paired.Token) != 64 {
		t.Fatalf("pair response: %+v", paired)
	}

	// Token stored hashed only.
	d, err := r.st.FindDeviceByTokenHash(cryptoutil.HashToken(paired.Token))
	if err != nil || d.ID != paired.DeviceID {
		t.Fatalf("hashed token lookup failed: %v", err)
	}

	// One-time code: reuse rejected.
	if code, _ = r.do(r.anon, "POST", "/api/v1/devices/pair",
		`{"code":"`+pc.Code+`","name":"dev2"}`); code != 400 {
		t.Fatal("code reuse accepted")
	}
	if code, _ = r.do(r.anon, "POST", "/api/v1/devices/pair",
		`{"code":"000000","name":"dev3"}`); code != 400 {
		t.Fatal("bogus code accepted")
	}

	// Revoke kicks the live conn and invalidates the token.
	if code, _ = r.do(r.auth, "DELETE", "/api/v1/devices/"+paired.DeviceID, ""); code != 200 {
		t.Fatal("revoke failed")
	}
	if len(r.ch.kicked) != 1 || r.ch.kicked[0] != paired.DeviceID {
		t.Fatalf("kick not called: %v", r.ch.kicked)
	}
	if _, err := r.st.FindDeviceByTokenHash(cryptoutil.HashToken(paired.Token)); err == nil {
		t.Fatal("revoked token still resolves")
	}
}
