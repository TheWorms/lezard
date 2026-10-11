package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type rec struct {
	msgs  []string
	chats []string
	fail  bool
}

func (r *rec) send(_, chat, t string) error {
	if r.fail {
		return errors.New("panne")
	}
	r.msgs = append(r.msgs, t)
	r.chats = append(r.chats, chat)
	return nil
}

func allEventsRecipient(chat string) Recipient {
	return Recipient{ID: "aaaaaaaa", Name: "Camille", ChatID: chat, Enabled: true, Events: append([]string{}, allEvents...), Hosts: []string{}}
}

func newAlertEnv(t *testing.T, dir string) (*Alerter, *Store, *clock, *rec) {
	t.Helper()
	ck := newClock()
	st, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, _ := loadConfig(env(map[string]string{"LEZARD_NAMES": "media=Média"}))
	ss, err := OpenSettings(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	set := ss.Get()
	set.Telegram = TelegramSettings{Enabled: true, Token: "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"}
	set.Recipients = []Recipient{allEventsRecipient("42")}
	if err := ss.Set(set); err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	a := NewAlerter(cfg, ss, st, r.send)
	a.now = ck.now
	a.started = ck.t.Add(-time.Hour) // hors période de grâce
	return a, st, ck, r
}

// step fait avancer l'horloge, pousse une valeur et évalue.
func step(a *Alerter, st *Store, ck *clock, host string, temp float64, d time.Duration) {
	ck.t = ck.t.Add(d)
	_ = st.AddAt(host, "cpu", temp, ck.t)
	a.Check()
}

func TestAlertLifecycle(t *testing.T) {
	a, st, ck, r := newAlertEnv(t, t.TempDir())
	step(a, st, ck, "media", 55, 0)
	if len(r.msgs) != 0 {
		t.Fatalf("aucune alerte attendue : %v", r.msgs)
	}
	// attention : confirmée seulement après LEZARD_ALERT_DELAY (1 min)
	step(a, st, ck, "media", 72, 20*time.Second)
	if len(r.msgs) != 0 {
		t.Fatalf("attention envoyée trop tôt : %v", r.msgs)
	}
	step(a, st, ck, "media", 72.5, 70*time.Second)
	if len(r.msgs) != 1 || !strings.Contains(r.msgs[0], "Média : attention, 72,5") {
		t.Fatalf("attention attendue : %v", r.msgs)
	}
	// critique : immédiat
	step(a, st, ck, "media", 81, 20*time.Second)
	if len(r.msgs) != 2 || !strings.Contains(r.msgs[1], "CRITIQUE") {
		t.Fatalf("critique attendu : %v", r.msgs)
	}
	// hystérésis : 79 reste critique (> 80 - 2)
	step(a, st, ck, "media", 79, 2*time.Minute)
	step(a, st, ck, "media", 79, 2*time.Minute)
	if len(r.msgs) != 2 {
		t.Fatalf("pas de redescente attendue à 79 °C : %v", r.msgs)
	}
	// 65 : retour à la normale (sous 70 - 2) après le délai
	step(a, st, ck, "media", 65, 20*time.Second)
	step(a, st, ck, "media", 65, 70*time.Second)
	if len(r.msgs) != 3 || !strings.Contains(r.msgs[2], "retour à la normale") {
		t.Fatalf("retour attendu : %v", r.msgs)
	}
	// hors ligne : plus de relevé pendant plus de 3 min
	ck.t = ck.t.Add(4 * time.Minute)
	a.Check()
	if len(r.msgs) != 4 || !strings.Contains(r.msgs[3], "hors ligne") {
		t.Fatalf("hors ligne attendu : %v", r.msgs)
	}
	step(a, st, ck, "media", 60, 10*time.Second)
	if len(r.msgs) != 4 {
		t.Fatalf("retour en ligne envoyé trop tôt : %v", r.msgs)
	}
	step(a, st, ck, "media", 60, 70*time.Second)
	if len(r.msgs) != 5 || !strings.Contains(r.msgs[4], "de nouveau en ligne") {
		t.Fatalf("retour en ligne attendu : %v", r.msgs)
	}
}

func TestAlertFlapIgnored(t *testing.T) {
	a, st, ck, r := newAlertEnv(t, t.TempDir())
	step(a, st, ck, "cuisine", 55, 0)
	step(a, st, ck, "cuisine", 71, 20*time.Second)
	step(a, st, ck, "cuisine", 66, 20*time.Second) // pic bref, retombé sous 68
	step(a, st, ck, "cuisine", 66, 2*time.Minute)
	if len(r.msgs) != 0 {
		t.Fatalf("un pic de 20 s ne doit pas alerter : %v", r.msgs)
	}
}

func TestAlertRetryAndGrouping(t *testing.T) {
	a, st, ck, r := newAlertEnv(t, t.TempDir())
	step(a, st, ck, "a1", 50, 0)
	step(a, st, ck, "a2", 50, 0)
	r.fail = true
	ck.t = ck.t.Add(20 * time.Second)
	_ = st.AddAt("a1", "cpu", 85, ck.t)
	_ = st.AddAt("a2", "cpu", 90, ck.t)
	a.Check()
	if a.errors != 1 || len(a.outbox) != 1 {
		t.Fatalf("échec non compté ou message perdu")
	}
	r.fail = false
	a.Check()
	if len(r.msgs) != 1 || strings.Count(r.msgs[0], "CRITIQUE") != 2 {
		t.Fatalf("un seul message groupé attendu après nouvel essai : %v", r.msgs)
	}
}

func TestAlertReminderAndPersistence(t *testing.T) {
	dir := t.TempDir()
	a, st, ck, r := newAlertEnv(t, dir)
	step(a, st, ck, "salon", 50, 0)
	step(a, st, ck, "salon", 85, 20*time.Second)
	for i := 0; i < 200; i++ { // 66 min
		step(a, st, ck, "salon", 85, 20*time.Second)
	}
	if len(r.msgs) != 2 || !strings.Contains(r.msgs[1], "Rappel") {
		t.Fatalf("un rappel après 1 h attendu : %v", r.msgs)
	}
	// redémarrage : l'état critique est relu, pas de nouvelle alerte
	a2 := NewAlerter(a.cfg, a.settings, st, r.send)
	a2.now = ck.now
	a2.started = ck.t
	step(a2, st, ck, "salon", 85, 20*time.Second)
	if len(r.msgs) != 2 {
		t.Fatalf("pas de nouvelle alerte attendue après redémarrage : %v", r.msgs)
	}
}

func TestAlertStartupGrace(t *testing.T) {
	a, st, ck, r := newAlertEnv(t, t.TempDir())
	step(a, st, ck, "garage", 40, 0)
	// Lézard redémarre 10 min plus tard : le dernier relevé paraît vieux
	ck.t = ck.t.Add(10 * time.Minute)
	a2 := NewAlerter(a.cfg, a.settings, st, r.send)
	a2.now = ck.now
	a2.started = ck.t
	a2.state = a.state
	a2.Check()
	if len(r.msgs) != 0 {
		t.Fatalf("pas de « hors ligne » pendant la période de grâce : %v", r.msgs)
	}
}

func TestRecipientFiltering(t *testing.T) {
	a, st, ck, r := newAlertEnv(t, t.TempDir())
	set := a.settings.Get()
	only := allEventsRecipient("100")
	only.ID, only.Name, only.Events, only.Hosts = "bbbbbbbb", "Critiques Cuisine", []string{evCritical}, []string{"cuisine"}
	quiet := allEventsRecipient("200")
	quiet.ID, quiet.Name = "cccccccc", "Nuit"
	quiet.QuietFrom, quiet.QuietTo, quiet.QuietCritical = "11:00", "13:00", true // il est 12:00
	off := allEventsRecipient("300")
	off.ID, off.Name, off.Enabled = "dddddddd", "Coupé", false
	set.Recipients = []Recipient{only, quiet, off}
	if err := a.settings.Set(set); err != nil {
		t.Fatal(err)
	}
	step(a, st, ck, "cuisine", 50, 0)
	step(a, st, ck, "salon", 50, 0)
	ck.t = ck.t.Add(20 * time.Second)
	_ = st.AddAt("cuisine", "cpu", 85, ck.t)
	_ = st.AddAt("salon", "cpu", 85, ck.t)
	a.Check()
	// "100" : seulement cuisine ; "200" : critiques passent la plage calme ; "300" : rien
	got := map[string]string{}
	for i, c := range r.chats {
		got[c] = r.msgs[i]
	}
	if len(got) != 2 || strings.Contains(got["100"], "Salon") || !strings.Contains(got["100"], "Cuisine") ||
		!strings.Contains(got["200"], "Salon") || !strings.Contains(got["200"], "Cuisine") {
		t.Fatalf("filtrage incorrect : %v", got)
	}
	// une alerte « attention » pendant la plage calme ne passe pas
	r.msgs, r.chats = nil, nil
	step(a, st, ck, "garage", 50, 0)
	step(a, st, ck, "garage", 72, 20*time.Second)
	step(a, st, ck, "garage", 72, 70*time.Second)
	for _, c := range r.chats {
		if c == "200" {
			t.Fatalf("attention envoyée pendant la plage calme")
		}
	}
}

func TestAlertsDisabledNoFlood(t *testing.T) {
	a, st, ck, r := newAlertEnv(t, t.TempDir())
	set := a.settings.Get()
	set.Telegram.Enabled = false
	_ = a.settings.Set(set)
	step(a, st, ck, "salon", 50, 0)
	step(a, st, ck, "salon", 85, 20*time.Second)
	set.Telegram.Enabled = true
	_ = a.settings.Set(set)
	step(a, st, ck, "salon", 85, 20*time.Second)
	if len(r.msgs) != 0 {
		t.Fatalf("activer les alertes ne doit pas renvoyer les anciens états : %v", r.msgs)
	}
}

func TestQuietWrapsMidnight(t *testing.T) {
	rc := Recipient{QuietFrom: "23:00", QuietTo: "07:00"}
	for h, want := range map[int]bool{22: false, 23: true, 2: true, 7: false, 12: false} {
		if got := rc.inQuiet(time.Date(2026, 1, 1, h, 30, 0, 0, time.UTC)); got != want {
			t.Errorf("%dh30 : %v, attendu %v", h, got, want)
		}
	}
}

func TestTelegramClient(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"maison_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"message":{"chat":{"id":42,"type":"private","first_name":"Camille"}}},{"message":{"chat":{"id":42,"type":"private","first_name":"Camille"}}},{"channel_post":{"chat":{"id":-1001,"type":"channel","title":"Alertes"}}}]}`))
		case got.Get("chat_id") == "bad":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	}))
	defer srv.Close()
	old := telegramAPI
	telegramAPI = srv.URL
	defer func() { telegramAPI = old }()

	if err := telegramSend("111:SECRET", "42", "bonjour"); err != nil || got.Get("text") != "bonjour" {
		t.Fatalf("envoi : %v %v", err, got)
	}
	if err := telegramSend("111:SECRET", "bad", "x"); err == nil || !strings.Contains(err.Error(), "chat not found") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("erreur attendue sans jeton : %v", err)
	}
	if name, err := telegramBotName("111:SECRET"); err != nil || name != "@maison_bot" {
		t.Fatalf("getMe : %q %v", name, err)
	}
	chats, err := telegramChats("111:SECRET")
	if err != nil || len(chats) != 2 {
		t.Fatalf("getUpdates : %v %v", chats, err)
	}
	telegramAPI = "http://127.0.0.1:1"
	if err := telegramSend("111:SECRET", "42", "x"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("le jeton ne doit pas fuiter dans l'erreur : %v", err)
	}
}
