package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Server expose l'API et la WebUI.
type Server struct {
	cfg          *Config
	store        *Store
	now          func() time.Time
	started      time.Time
	tokenHash    [32]byte
	static       fs.FS
	alerter      *Alerter
	settings     *SettingsStore
	auth         *adminAuth
	refused      logLimiter
	refusedLogin logLimiter
}

// logLimiter limite un message de journal répétitif à un par minute, avec le
// nombre d'occurrences regroupées (évite qu'un client mal réglé remplisse le disque).
type logLimiter struct {
	mu     sync.Mutex
	last   time.Time
	missed int
}

func (l *logLimiter) logf(now time.Time, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) < time.Minute {
		l.missed++
		return
	}
	if l.missed > 0 {
		format += " (+%d refus similaires dans la dernière minute)"
		args = append(args, l.missed)
	}
	log.Printf(format, args...)
	l.last, l.missed = now, 0
}

// NewServer prépare le routeur.
func NewServer(cfg *Config, store *Store, settings *SettingsStore) *Server {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	s := &Server{cfg: cfg, store: store, settings: settings, now: time.Now, static: sub, auth: newAdminAuth(cfg.AdminPassword)}
	s.started = s.now()
	s.tokenHash = sha256.Sum256([]byte(cfg.Token))
	return s
}

// Handler renvoie le routeur HTTP avec les en-têtes de sécurité.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/push", s.handlePush)
	mux.HandleFunc("GET /api/hosts", s.handleHosts)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/history/all", s.handleHistoryAll)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/settings", s.admin(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.admin(s.handlePutSettings))
	mux.HandleFunc("POST /api/telegram/test", s.admin(s.handleTelegramTest))
	mux.HandleFunc("POST /api/telegram/chats", s.admin(s.handleTelegramChats))
	mux.HandleFunc("GET /overview.js", s.serveAsset("overview.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /settings.js", s.serveAsset("settings.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /{$}", s.serveAsset("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /theme.js", s.serveAsset("theme.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.serveAsset("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /icon.svg", s.serveAsset("icon.svg", "image/svg+xml"))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) serveAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(s.static, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) authorized(r *http.Request) bool {
	got := r.Header.Get("X-Lezard-Token")
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if got == "" {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(sum[:], s.tokenHash[:]) == 1
}

func parseTempValue(v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), ",", "."), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("temp doit être un nombre en °C")
	}
	if f < -50 || f > 150 {
		return 0, errors.New("temp hors plage plausible (-50 à 150 °C) : relevé ignoré")
	}
	return f, nil
}

// handlePush reçoit un relevé : POST /api/push avec host, temp et sensor facultatif.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.refused.logf(s.now(), "push refusé (jeton absent ou invalide) depuis %s", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "jeton absent ou invalide")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "corps trop volumineux")
			return
		}
		writeError(w, http.StatusBadRequest, "formulaire illisible")
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	sensor := strings.ToLower(strings.TrimSpace(r.FormValue("sensor")))
	if sensor == "" {
		sensor = "cpu"
	}
	if !nameRe.MatchString(host) {
		writeError(w, http.StatusBadRequest, "host invalide : 1 à 40 caractères parmi a-z 0-9 . _ -")
		return
	}
	if !nameRe.MatchString(sensor) {
		writeError(w, http.StatusBadRequest, "sensor invalide : 1 à 40 caractères parmi a-z 0-9 . _ -")
		return
	}
	temp, err := parseTempValue(r.FormValue("temp"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.AddLimited(host, sensor, temp, s.cfg.MaxSeries); err != nil {
		if errors.Is(err, errTooManySeries) {
			writeError(w, http.StatusConflict, "nombre maximal de séries atteint")
			return
		}
		log.Printf("écriture du relevé %s/%s : %v", host, sensor, err)
		writeError(w, http.StatusInternalServerError, "écriture impossible")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHealth : 200 si tout va bien, 503 (« degraded ») si une écriture disque
// a échoué ces 5 dernières minutes (disque plein, volume en lecture seule…), pour
// qu'une sonde externe le signale.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	var alerts any
	if s.alerter != nil {
		alerts = s.alerter.Stats()
	}
	errs, lastErr := s.store.WriteErrors()
	code, status := http.StatusOK, "ok"
	if !lastErr.IsZero() && s.now().Sub(lastErr) < 5*time.Minute {
		code, status = http.StatusServiceUnavailable, "degraded"
	}
	writeJSON(w, code, map[string]any{
		"alerts":       alerts,
		"status":       status,
		"version":      version,
		"series":       s.store.SeriesCount(),
		"write_errors": errs,
		"uptime":       int64(s.now().Sub(s.started).Seconds()),
	})
}

var ranges = map[string]int64{
	"1h":  3600,
	"6h":  6 * 3600,
	"24h": 24 * 3600,
	"3d":  3 * 86400,
	"7d":  7 * 86400,
	"30d": 30 * 86400,
	"90d": 90 * 86400,
	"1y":  365 * 86400,
}

func rangeParam(r *http.Request) (string, int64) {
	name := r.URL.Query().Get("range")
	if secs, ok := ranges[name]; ok {
		return name, secs
	}
	return "24h", ranges["24h"]
}

func round(x float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(x*p) / p
}

type sensorOut struct {
	Sensor string       `json:"sensor"`
	Status string       `json:"status"`
	Temp   float64      `json:"temp"`
	Last   int64        `json:"last"`
	Min    *float64     `json:"min,omitempty"`
	Max    *float64     `json:"max,omitempty"`
	Avg    *float64     `json:"avg,omitempty"`
	Spark  [][2]float64 `json:"spark"`
}

type hostOut struct {
	Host    string      `json:"host"`
	Name    string      `json:"name"`
	Color   int         `json:"color"`
	Status  string      `json:"status"`
	Temp    float64     `json:"temp"`
	Last    int64       `json:"last"`
	Age     int64       `json:"age"`
	Warn    float64     `json:"warn"`
	Crit    float64     `json:"crit"`
	Sensors []sensorOut `json:"sensors"`
}

// handleHosts : GET /api/hosts?range=24h, l'état de chaque machine.
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	rangeName, rangeSecs := rangeParam(r)
	now := s.now()
	nowT := now.Unix()
	s.assignColors()
	set := s.settings.Get()
	views := s.store.Overview(now, nowT-int64(s.cfg.Forget/time.Second), rangeSecs, niceStep(rangeSecs, 60))

	byHost := map[string]*hostOut{}
	for _, v := range views {
		h := byHost[v.Host]
		if h == nil {
			warn, crit := set.thresholds(v.Host)
			h = &hostOut{Host: v.Host, Name: set.displayName(v.Host), Color: set.Hosts[v.Host].Color, Warn: warn, Crit: crit, Sensors: []sensorOut{}}
			byHost[v.Host] = h
		}
		so := sensorOut{Sensor: v.Sensor, Temp: round(v.LastC, 1), Last: v.LastT, Spark: make([][2]float64, 0, len(v.Spark))}
		if v.HasRng {
			mn, mx, av := round(v.Min, 1), round(v.Max, 1), round(v.Avg, 1)
			so.Min, so.Max, so.Avg = &mn, &mx, &av
		}
		for _, row := range v.Spark {
			so.Spark = append(so.Spark, [2]float64{float64(row.T), round(row.Avg, 2)})
		}
		h.Sensors = append(h.Sensors, so)
	}

	hosts := make([]hostOut, 0, len(byHost))
	summary := map[string]int{statusOK: 0, statusWarning: 0, statusCritical: 0, statusOffline: 0}
	staleSecs := int64(s.cfg.Stale / time.Second)
	for _, h := range byHost {
		sort.Slice(h.Sensors, func(i, j int) bool {
			a, b := h.Sensors[i].Sensor, h.Sensors[j].Sensor
			if (a == "cpu") != (b == "cpu") {
				return a == "cpu"
			}
			return a < b
		})
		for _, so := range h.Sensors {
			if so.Last > h.Last {
				h.Last = so.Last
			}
		}
		h.Age = nowT - h.Last
		if h.Age < 0 {
			h.Age = 0
		}
		// température de la machine : capteur actif le plus chaud (un capteur
		// secondaire muet ne doit pas afficher une vieille valeur)
		hottest, live := h.Sensors[0].Temp, false
		for i := range h.Sensors {
			so := &h.Sensors[i]
			if nowT-so.Last > staleSecs {
				so.Status = statusOffline
				continue
			}
			so.Status = statusFor(so.Temp, h.Warn, h.Crit)
			if !live || so.Temp > hottest {
				hottest, live = so.Temp, true
			}
		}
		h.Temp = hottest
		if h.Age > staleSecs {
			h.Status = statusOffline
		} else {
			// machine en ligne : son statut vient de ses capteurs actifs (un capteur
			// secondaire muet reste signalé sur sa ligne, sans rendre la machine hors ligne)
			h.Status = statusOK
			for _, so := range h.Sensors {
				if so.Status != statusOffline && severity(so.Status) > severity(h.Status) {
					h.Status = so.Status
				}
			}
		}
		summary[h.Status]++
		hosts = append(hosts, *h)
	}
	sort.Slice(hosts, func(i, j int) bool {
		return strings.ToLower(hosts[i].Name) < strings.ToLower(hosts[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"now":     nowT,
		"range":   rangeName,
		"stale":   staleSecs,
		"summary": summary,
		"hosts":   hosts,
	})
}

type historySeries struct {
	Sensor string       `json:"sensor"`
	Rows   [][5]float64 `json:"rows"`
}

// handleHistory : GET /api/history?host=x&range=24h[&sensor=cpu].
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.URL.Query().Get("host"))
	if !nameRe.MatchString(host) {
		writeError(w, http.StatusBadRequest, "host manquant ou invalide")
		return
	}
	sensors := s.store.Sensors(host)
	if len(sensors) == 0 {
		writeError(w, http.StatusNotFound, "machine inconnue")
		return
	}
	if want := strings.ToLower(r.URL.Query().Get("sensor")); want != "" {
		found := false
		for _, name := range sensors {
			if name == want {
				found = true
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "capteur inconnu")
			return
		}
		sensors = []string{want}
	}
	sort.Slice(sensors, func(i, j int) bool {
		if (sensors[i] == "cpu") != (sensors[j] == "cpu") {
			return sensors[i] == "cpu"
		}
		return sensors[i] < sensors[j]
	})
	rangeName, rangeSecs := rangeParam(r)
	now := s.now()
	step := niceStep(rangeSecs, 240)
	since := now.Add(-time.Duration(rangeSecs) * time.Second)
	out := make([]historySeries, 0, len(sensors))
	for _, name := range sensors {
		rows := s.store.History(host, name, since, step)
		hs := historySeries{Sensor: name, Rows: make([][5]float64, 0, len(rows))}
		for _, row := range rows {
			hs.Rows = append(hs.Rows, [5]float64{float64(row.T), round(row.Avg, 2), round(row.Min, 2), round(row.Max, 2), float64(row.N)})
		}
		out = append(out, hs)
	}
	set := s.settings.Get()
	warn, crit := set.thresholds(host)
	writeJSON(w, http.StatusOK, map[string]any{
		"host":    host,
		"name":    set.displayName(host),
		"range":   rangeName,
		"since":   since.Unix(),
		"until":   now.Unix(),
		"step":    step,
		"warn":    warn,
		"crit":    crit,
		"sensors": out,
	})
}

// assignColors attribue une couleur aux machines visibles qui n'en ont pas.
func (s *Server) assignColors() {
	seen := map[string]bool{}
	var hosts []string
	for _, l := range s.store.LatestAll(s.now().Unix() - int64(s.cfg.Forget/time.Second)) {
		if !seen[l.Host] {
			seen[l.Host] = true
			hosts = append(hosts, l.Host)
		}
	}
	s.settings.AssignColors(hosts)
}

type allHost struct {
	Host    string          `json:"host"`
	Name    string          `json:"name"`
	Color   int             `json:"color"`
	Warn    float64         `json:"warn"`
	Crit    float64         `json:"crit"`
	Sensors []historySeries `json:"sensors"`
}

// handleHistoryAll : GET /api/history/all?range=24h, la courbe de chaque capteur
// de chaque machine sur la même grille de temps (graphique « toutes les machines »).
func (s *Server) handleHistoryAll(w http.ResponseWriter, r *http.Request) {
	s.assignColors()
	rangeName, rangeSecs := rangeParam(r)
	now := s.now()
	step := niceStep(rangeSecs, 240)
	since := now.Add(-time.Duration(rangeSecs) * time.Second)
	set := s.settings.Get()
	seen := map[string]bool{}
	var names []string
	for _, l := range s.store.LatestAll(now.Unix() - int64(s.cfg.Forget/time.Second)) {
		if !seen[l.Host] {
			seen[l.Host] = true
			names = append(names, l.Host)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(set.displayName(names[i])) < strings.ToLower(set.displayName(names[j]))
	})
	out := make([]allHost, 0, len(names))
	for _, host := range names {
		sensors := s.store.Sensors(host)
		sort.Slice(sensors, func(i, j int) bool {
			if (sensors[i] == "cpu") != (sensors[j] == "cpu") {
				return sensors[i] == "cpu"
			}
			return sensors[i] < sensors[j]
		})
		warn, crit := set.thresholds(host)
		h := allHost{Host: host, Name: set.displayName(host), Color: set.Hosts[host].Color, Warn: warn, Crit: crit, Sensors: []historySeries{}}
		for _, name := range sensors {
			rows := s.store.History(host, name, since, step)
			hs := historySeries{Sensor: name, Rows: make([][5]float64, 0, len(rows))}
			for _, row := range rows {
				hs.Rows = append(hs.Rows, [5]float64{float64(row.T), round(row.Avg, 2), round(row.Min, 2), round(row.Max, 2), float64(row.N)})
			}
			h.Sensors = append(h.Sensors, hs)
		}
		out = append(out, h)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rangeName, "since": since.Unix(), "until": now.Unix(), "step": step, "hosts": out,
	})
}
