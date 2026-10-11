package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testToken = "0123456789abcdef-token"
	testAdmin = "mot-de-passe-admin"
)

func newTestServer(t *testing.T) (*Server, *Store, *clock) {
	t.Helper()
	ck := newClock()
	st, err := OpenStore(t.TempDir(), StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, _ := loadConfig(env(map[string]string{"LEZARD_TOKEN": testToken, "LEZARD_ADMIN_PASSWORD": testAdmin}))
	ss, err := OpenSettings(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, st, ss)
	s.now = ck.now
	return s, st, ck
}

func push(s *Server, auth string, form url.Values) int {
	r := httptest.NewRequest("POST", "/api/push", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}

func TestPushAuthAndValidation(t *testing.T) {
	s, st, _ := newTestServer(t)
	ok := url.Values{"host": {"Pi4"}, "temp": {"48,5"}}
	if c := push(s, "", ok); c != 401 {
		t.Errorf("sans jeton : %d", c)
	}
	if c := push(s, "Bearer mauvais", ok); c != 401 {
		t.Errorf("mauvais jeton : %d", c)
	}
	if c := push(s, "Bearer "+testToken, ok); c != 204 {
		t.Errorf("valide : %d", c)
	}
	if !st.HasSeries("pi4", "cpu") {
		t.Error("hôte non normalisé en minuscules / capteur cpu par défaut")
	}
	bad := []url.Values{
		{"host": {"../etc"}, "temp": {"40"}},
		{"host": {"pi"}, "temp": {"abc"}},
		{"host": {"pi"}, "temp": {"-263"}},
		{"host": {"pi"}, "temp": {"NaN"}},
		{"host": {"pi"}, "temp": {"40"}, "sensor": {"A B"}},
		{"temp": {"40"}},
	}
	for i, f := range bad {
		if c := push(s, "Bearer "+testToken, f); c != 400 {
			t.Errorf("cas %d : %d, attendu 400", i, c)
		}
	}
}

func TestPushMaxSeries(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.cfg.MaxSeries = 2
	for _, h := range []string{"a1", "a2"} {
		if c := push(s, "Bearer "+testToken, url.Values{"host": {h}, "temp": {"40"}}); c != 204 {
			t.Fatalf("%s : %d", h, c)
		}
	}
	if c := push(s, "Bearer "+testToken, url.Values{"host": {"a3"}, "temp": {"40"}}); c != 409 {
		t.Errorf("limite : %d", c)
	}
	if c := push(s, "Bearer "+testToken, url.Values{"host": {"a1"}, "temp": {"41"}}); c != 204 {
		t.Errorf("série existante : %d", c)
	}
}

func get(s *Server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestHostsStatuses(t *testing.T) {
	s, st, ck := newTestServer(t)
	_ = st.AddAt("chaud", "cpu", 75, ck.t.Add(-10*time.Second))
	_ = st.AddAt("brulant", "cpu", 85, ck.t.Add(-10*time.Second))
	_ = st.AddAt("frais", "cpu", 40, ck.t.Add(-10*time.Second))
	_ = st.AddAt("mort", "cpu", 40, ck.t.Add(-10*time.Minute))
	_ = st.AddAt("oublie", "cpu", 40, ck.t.Add(-40*24*time.Hour))
	w := get(s, "/api/hosts?range=1h")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var d struct {
		Summary map[string]int `json:"summary"`
		Hosts   []struct {
			Host, Status string
		} `json:"hosts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, h := range d.Hosts {
		got[h.Host] = h.Status
	}
	want := map[string]string{"chaud": "warning", "brulant": "critical", "frais": "ok", "mort": "offline"}
	if len(got) != len(want) {
		t.Fatalf("hôtes : %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s : %s, attendu %s", k, got[k], v)
		}
	}
}

func TestHistoryAndErrors(t *testing.T) {
	s, st, ck := newTestServer(t)
	for i := 0; i < 20; i++ {
		_ = st.AddAt("pi", "cpu", 50, ck.t.Add(time.Duration(i-20)*time.Minute))
	}
	if w := get(s, "/api/history?host=pi&range=1h"); w.Code != 200 || !strings.Contains(w.Body.String(), `"rows"`) {
		t.Errorf("historique : %d %s", w.Code, w.Body.String())
	}
	if w := get(s, "/api/history?host=inconnu"); w.Code != 404 {
		t.Errorf("inconnu : %d", w.Code)
	}
	if w := get(s, "/api/history?host=%2e%2e"); w.Code != 400 {
		t.Errorf("invalide : %d", w.Code)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, p := range []string{"/", "/app.js", "/app.css", "/theme.js", "/icon.svg", "/api/health"} {
		w := get(s, p)
		if w.Code != http.StatusOK {
			t.Errorf("%s : %d", p, w.Code)
		}
		if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s : en-têtes de sécurité manquants", p)
		}
	}
	if w := get(s, "/nimporte"); w.Code != 404 {
		t.Errorf("404 attendu : %d", w.Code)
	}
}
