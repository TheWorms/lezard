package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests des protections ajoutées lors de l'audit : limites, pannes disque et
// Telegram, robustesse des tâches de fond.

func TestMaxSeriesIsAtomic(t *testing.T) {
	ck := newClock()
	st, err := OpenStore(t.TempDir(), StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = st.AddLimited("h"+string(rune('a'+i%26))+string(rune('a'+i/26)), "cpu", 40, 10)
		}(i)
	}
	wg.Wait()
	if n := st.SeriesCount(); n != 10 {
		t.Fatalf("limite de 10 séries dépassée en concurrence : %d", n)
	}
	// une série existante accepte toujours ses relevés
	if err := st.AddLimited("ha", "cpu", 41, 10); err != nil && st.HasSeries("ha", "cpu") {
		t.Fatalf("série existante refusée : %v", err)
	}
}

func TestEmptySeriesAreDropped(t *testing.T) {
	ck := newClock()
	st, err := OpenStore(t.TempDir(), StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.Add("vieux", "cpu", 40)
	// au-delà de toutes les rétentions (400 j d'agrégats horaires)
	ck.t = ck.t.Add(401 * 24 * time.Hour)
	st.Maintain()
	if st.HasSeries("vieux", "cpu") {
		t.Fatal("une série sans aucune donnée doit être retirée (sinon elle bloque la limite)")
	}
}

func TestHealthDegradedOnWriteError(t *testing.T) {
	s, st, ck := newTestServer(t)
	get := func() (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
		var body struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Status
	}
	if c, stt := get(); c != 200 || stt != "ok" {
		t.Fatalf("santé attendue ok : %d %s", c, stt)
	}
	// simule un disque en panne : le fichier du jour est fermé sous le stockage
	if c := push(s, "Bearer "+testToken, url.Values{"host": {"pi"}, "temp": {"40"}}); c != 204 {
		t.Fatalf("push initial : %d", c)
	}
	st.mu.Lock()
	_ = st.rawF.Close()
	st.mu.Unlock()
	if c := push(s, "Bearer "+testToken, url.Values{"host": {"pi"}, "temp": {"41"}}); c != 500 {
		t.Fatalf("écriture en échec attendue (500) : %d", c)
	}
	if c, stt := get(); c != 503 || stt != "degraded" {
		t.Fatalf("santé dégradée attendue : %d %s", c, stt)
	}
	ck.t = ck.t.Add(6 * time.Minute)
	if c, _ := get(); c != 200 {
		t.Fatalf("santé rétablie attendue après 5 min sans erreur : %d", c)
	}
}

func TestHostTempIgnoresStaleSensor(t *testing.T) {
	s, st, ck := newTestServer(t)
	_ = st.AddAt("nas", "disque", 90, ck.t)
	ck.t = ck.t.Add(10 * time.Minute)
	_ = st.AddAt("nas", "cpu", 45, ck.t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/hosts", nil))
	var out struct {
		Hosts []hostOut `json:"hosts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Hosts) != 1 {
		t.Fatalf("réponse : %v %s", err, w.Body.String())
	}
	if h := out.Hosts[0]; h.Temp != 45 || h.Status != statusOK {
		t.Fatalf("le capteur muet ne doit pas donner la température : %+v", h)
	}
}

func TestRefusedPushLogIsRateLimited(t *testing.T) {
	var l logLimiter
	now := time.Unix(1_700_000_000, 0)
	l.logf(now, "x")
	for i := 0; i < 100; i++ {
		l.logf(now.Add(time.Second), "x")
	}
	if l.missed != 100 {
		t.Fatalf("les refus répétés doivent être regroupés : %d", l.missed)
	}
	l.logf(now.Add(2*time.Minute), "x")
	if l.missed != 0 {
		t.Fatal("le compteur doit repartir après une ligne de journal")
	}
}

func TestSplitLines(t *testing.T) {
	line := strings.Repeat("é", 40) // 80 octets
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, line)
	}
	msgs := splitLines(lines)
	if len(msgs) < 2 {
		t.Fatalf("un long message doit être découpé : %d", len(msgs))
	}
	total := 0
	for _, m := range msgs {
		if len(m) > maxMsgLen {
			t.Fatalf("message trop long : %d", len(m))
		}
		total += strings.Count(m, "\n") + 1
	}
	if total != 200 {
		t.Fatalf("lignes perdues : %d", total)
	}
	long := splitLines([]string{strings.Repeat("é", 3000)})
	if len(long) != 1 || len(long[0]) > maxMsgLen || !strings.HasPrefix(long[0], "é") || strings.ContainsRune(long[0], '�') {
		t.Fatal("une ligne trop longue doit être tronquée proprement")
	}
}

func TestPermanentTelegramErrorIsDropped(t *testing.T) {
	a, st, ck, _ := newAlertEnv(t, t.TempDir())
	calls := 0
	a.send = func(_, _, _ string) error {
		calls++
		return &tgError{msg: "chat not found", permanent: true}
	}
	step(a, st, ck, "salon", 50, 0)
	step(a, st, ck, "salon", 85, 20*time.Second)
	step(a, st, ck, "salon", 85, 20*time.Second)
	if calls != 1 {
		t.Fatalf("un refus définitif ne doit pas être retenté : %d appels", calls)
	}
	if p := a.Stats()["pending"]; p != 0 {
		t.Fatalf("file d'envoi non vidée : %v", p)
	}
}

func TestNetworkErrorStopsPassAndRetries(t *testing.T) {
	a, st, ck, _ := newAlertEnv(t, t.TempDir())
	set := a.settings.Get()
	set.Recipients = append(set.Recipients, allEventsRecipient("43"))
	set.Recipients[1].ID = "bbbbbbbb"
	if err := a.settings.Set(set); err != nil {
		t.Fatal(err)
	}
	calls, down := 0, true
	a.send = func(_, _, _ string) error {
		calls++
		if down {
			return &tgError{msg: "injoignable", network: true}
		}
		return nil
	}
	step(a, st, ck, "salon", 50, 0)
	step(a, st, ck, "salon", 85, 20*time.Second)
	if calls != 1 {
		t.Fatalf("après une panne réseau, le passage doit s'arrêter : %d appels", calls)
	}
	down = false
	step(a, st, ck, "salon", 85, 20*time.Second)
	if calls != 3 || a.Stats()["pending"] != 0 || a.Stats()["sent"] != 2 {
		t.Fatalf("les deux messages doivent partir au passage suivant : %d appels, %v", calls, a.Stats())
	}
}

func TestHealthNotBlockedBySlowTelegram(t *testing.T) {
	a, st, ck, _ := newAlertEnv(t, t.TempDir())
	release := make(chan struct{})
	a.send = func(_, _, _ string) error {
		<-release
		return nil
	}
	step(a, st, ck, "salon", 50, 0)
	ck.t = ck.t.Add(20 * time.Second)
	_ = st.AddAt("salon", "cpu", 85, ck.t)
	done := make(chan struct{})
	go func() { a.Check(); close(done) }()
	time.Sleep(50 * time.Millisecond) // l'envoi est en cours et bloqué
	got := make(chan map[string]any, 1)
	go func() { got <- a.Stats() }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("Stats (donc /api/health) bloqué pendant un envoi Telegram")
	}
	close(release)
	<-done
}

func TestSafeRunRecovers(t *testing.T) {
	safeRun("test", func() { panic("boum") })
}

func TestRecipientIDMustBeHex(t *testing.T) {
	s := &Settings{Rules: Rules{Warn: 70, Crit: 80}, Recipients: []Recipient{
		{ID: "<script>", Name: "A", ChatID: "1", Enabled: true},
	}}
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	if !idRe.MatchString(s.Recipients[0].ID) {
		t.Fatalf("identifiant non conforme conservé : %q", s.Recipients[0].ID)
	}
}

func TestStartupSurvivesCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	junk := strings.Repeat("\x00", 2<<20) // 2 Mo sans saut de ligne, comme après une coupure de courant
	good := "1700000000\tnas\tcpu\t50.000\t45.00\t55.00\t10\n"
	for name, content := range map[string]string{
		"m10.tsv":          good + junk + "\n" + good,
		"h1.tsv":           junk,
		"raw-20991231.tsv": "4102444800\tnas\tcpu\t41.00\n" + junk + "\n4102444860\tnas\tcpu\t42.00",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ck := newClock()
	st, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatalf("un fichier abîmé ne doit pas empêcher le démarrage : %v", err)
	}
	defer st.Close()
	if !st.HasSeries("nas", "cpu") {
		t.Fatal("les lignes valides doivent être conservées")
	}
}

func TestPartialLineAfterWriteFailureIsTerminated(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	st, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.AddAt("nas", "cpu", 40, ck.t)
	st.mu.Lock()
	_ = st.rawF.Close() // les écritures suivantes échouent
	st.mu.Unlock()
	ck.t = ck.t.Add(time.Minute)
	if err := st.AddAt("nas", "cpu", 41, ck.t); err == nil {
		t.Fatal("écriture en échec attendue")
	}
	ck.t = ck.t.Add(time.Minute)
	if err := st.AddAt("nas", "cpu", 42, ck.t); err != nil {
		t.Fatalf("le stockage doit rouvrir son fichier après une panne : %v", err)
	}
	st.Close()
	st2, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	rows := st2.History("nas", "cpu", ck.t.Add(-time.Hour), 0)
	if len(rows) != 2 || rows[1].Avg != 42 {
		t.Fatalf("relevés rechargés : %+v", rows)
	}
}

func TestDuplicateReadingInSameSecondNotWrittenTwice(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	st, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 50; i++ {
		_ = st.AddLimited("nas", "cpu", 40, 10)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "raw-*.tsv"))
	b, _ := os.ReadFile(files[0])
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("un même relevé répété doit être écrit une fois : %d lignes", n)
	}
}

func TestOutboxSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a, st, ck, _ := newAlertEnv(t, dir)
	a.send = func(_, _, _ string) error { return &tgError{msg: "injoignable", network: true} }
	step(a, st, ck, "salon", 50, 0)
	step(a, st, ck, "salon", 85, 20*time.Second)
	if a.Stats()["pending"] != 1 {
		t.Fatalf("un message doit attendre : %v", a.Stats())
	}
	// redémarrage : nouvel Alerter sur les mêmes fichiers, Telegram de nouveau joignable
	var got []string
	a2 := NewAlerter(a.cfg, a.settings, st, func(_, chat, text string) error {
		got = append(got, chat+"|"+text)
		return nil
	})
	a2.now = ck.now
	a2.started = ck.t.Add(-time.Hour)
	ck.t = ck.t.Add(20 * time.Second)
	_ = st.AddAt("salon", "cpu", 85, ck.t)
	a2.Check()
	if len(got) != 1 || !strings.Contains(got[0], "CRITIQUE") {
		t.Fatalf("l'alerte en attente doit partir après le redémarrage : %v", got)
	}
	a3 := NewAlerter(a.cfg, a.settings, st, func(_, _, _ string) error { t.Fatal("rien ne doit être renvoyé"); return nil })
	a3.now = ck.now
	if len(a3.outbox) != 0 {
		t.Fatalf("file à vider après livraison : %v", a3.outbox)
	}
}

func TestLoginLimitHoldsUnderParallelAttempts(t *testing.T) {
	s, _, _ := newTestServer(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"faux-faux-faux"}`))
			r.Header.Set("X-Lezard", "1")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			mu.Lock()
			codes[w.Code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[401] != 5 || codes[429] != 35 {
		t.Fatalf("5 essais au plus doivent être testés, même en parallèle : %v", codes)
	}
}

func TestLoginSuccessResetsCounter(t *testing.T) {
	s, _, _ := newTestServer(t)
	login := func(pw string) int {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+pw+`"}`))
		r.Header.Set("X-Lezard", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 3; i++ {
		_ = login("faux-faux-faux")
	}
	if c := login(testAdmin); c != 204 {
		t.Fatalf("bon mot de passe : %d", c)
	}
	for i := 0; i < 4; i++ {
		if c := login("faux-faux-faux"); c != 401 {
			t.Fatalf("le compteur doit repartir à zéro après une connexion réussie : %d", c)
		}
	}
}

func TestHealthcheckTargetsListenHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://") // 127.0.0.1:port
	if c := runHealthcheck(addr); c != 0 {
		t.Fatalf("sonde sur une IP précise : %d", c)
	}
	_, port, _ := net.SplitHostPort(addr)
	if c := runHealthcheck(":" + port); c != 0 {
		t.Fatalf("sonde sur toutes les interfaces : %d", c)
	}
}

func TestHealthHidesTelegramErrorText(t *testing.T) {
	s, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
	if strings.Contains(w.Body.String(), "last_error") {
		t.Fatal("/api/health est public : le texte des erreurs Telegram n'y a pas sa place")
	}
}
