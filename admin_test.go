package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(s *Server, method, path, body string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if csrf {
		r.Header.Set("X-Lezard", "1")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func login(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	w := do(s, "POST", "/api/login", `{"password":"`+testAdmin+`"}`, nil, true)
	if w.Code != 204 {
		t.Fatalf("connexion : %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatal("cookie sans HttpOnly/SameSite")
			}
			return c
		}
	}
	t.Fatal("pas de cookie de session")
	return nil
}

func TestAdminAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	if w := do(s, "GET", "/api/settings", "", nil, false); w.Code != 401 {
		t.Errorf("réglages sans session : %d", w.Code)
	}
	if w := do(s, "POST", "/api/login", `{"password":"`+testAdmin+`"}`, nil, false); w.Code != 403 {
		t.Errorf("connexion sans X-Lezard : %d", w.Code)
	}
	c := login(t, s)
	if w := do(s, "GET", "/api/settings", "", c, false); w.Code != 200 {
		t.Errorf("réglages avec session : %d", w.Code)
	}
	if w := do(s, "PUT", "/api/settings", `{}`, c, false); w.Code != 403 {
		t.Errorf("écriture sans X-Lezard : %d", w.Code)
	}
	forged := &http.Cookie{Name: sessionCookie, Value: "99999999999.deadbeef"}
	if w := do(s, "GET", "/api/settings", "", forged, false); w.Code != 401 {
		t.Errorf("cookie forgé accepté : %d", w.Code)
	}
}

func TestAdminLockout(t *testing.T) {
	s, _, _ := newTestServer(t)
	for i := 0; i < 5; i++ {
		if w := do(s, "POST", "/api/login", `{"password":"faux-faux-faux"}`, nil, true); w.Code != 401 {
			t.Fatalf("essai %d : %d", i, w.Code)
		}
	}
	if w := do(s, "POST", "/api/login", `{"password":"`+testAdmin+`"}`, nil, true); w.Code != 429 {
		t.Errorf("verrouillage attendu : %d", w.Code)
	}
}

func TestAdminDisabledWithoutPassword(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.auth = newAdminAuth("")
	if w := do(s, "POST", "/api/login", `{"password":""}`, nil, true); w.Code != 403 {
		t.Errorf("connexion sans mot de passe configuré : %d", w.Code)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s, st, ck := newTestServer(t)
	_ = st.AddAt("serveur", "cpu", 50, ck.t)
	c := login(t, s)
	body := `{
	  "telegram": {"enabled": false, "token": "", "clear_token": false},
	  "recipients": [{"id":"","name":"Camille","chat_id":"123456789","enabled":true,"events":["warning","critical"],"hosts":[],"quiet_from":"23:00","quiet_to":"07:00","quiet_critical":true}],
	  "rules": {"warn": 65, "crit": 78, "delay_min": 2, "repeat_min": 30, "hyst": 3},
	  "hosts": [{"host":"serveur","name":"Serveur","warn":80,"crit":88,"seen":true}]
	}`
	w := do(s, "PUT", "/api/settings", body, c, true)
	if w.Code != 200 {
		t.Fatalf("enregistrement : %d %s", w.Code, w.Body.String())
	}
	var out settingsOut
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Recipients) != 1 || len(out.Recipients[0].ID) != 8 || out.Recipients[0].Events[0] != "critical" {
		t.Errorf("destinataire : %+v", out.Recipients)
	}
	if out.Rules.Warn != 65 || len(out.Hosts) != 1 || out.Hosts[0].Name != "Serveur" {
		t.Errorf("règles/machines : %+v %+v", out.Rules, out.Hosts)
	}
	// les seuils et noms s'appliquent à l'affichage
	h := get(s, "/api/hosts?range=1h").Body.String()
	if !strings.Contains(h, `"name":"Serveur"`) || !strings.Contains(h, `"warn":80`) {
		t.Errorf("réglages non appliqués : %s", h)
	}
	bad := []string{
		strings.Replace(body, `"warn": 65`, `"warn": 90`, 1),                 // warn >= crit
		strings.Replace(body, `"chat_id":"123456789"`, `"chat_id":"abc"`, 1), // chat invalide
		strings.Replace(body, `"enabled": false`, `"enabled": true`, 1),      // activer sans jeton
		strings.Replace(body, `"quiet_to":"07:00"`, `"quiet_to":"25:00"`, 1), // heure invalide
		strings.Replace(body, `"events":["warning","critical"]`, `"events":["x"]`, 1),
		strings.Replace(body, `"token": ""`, `"token": "pas-un-jeton"`, 1),
		strings.Replace(body, `"rules"`, `"inconnu": 1, "rules"`, 1), // champ inconnu
	}
	for i, b := range bad {
		if w := do(s, "PUT", "/api/settings", b, c, true); w.Code != 400 {
			t.Errorf("cas %d : %d, attendu 400", i, w.Code)
		}
	}
	// le jeton n'est jamais renvoyé en clair
	set := s.settings.Get()
	set.Telegram.Token = "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdWXYZ"
	_ = s.settings.Set(set)
	g := do(s, "GET", "/api/settings", "", c, false).Body.String()
	if strings.Contains(g, "ABCDEFGHIJ") || !strings.Contains(g, `"token_hint":"…WXYZ"`) {
		t.Errorf("jeton exposé ou indice absent : %s", g)
	}
}
