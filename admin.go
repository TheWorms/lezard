package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Administration : page Réglages protégée par LEZARD_ADMIN_PASSWORD.
// Session = cookie signé (HMAC, clé tirée au démarrage), HttpOnly, SameSite=Strict,
// 12 h. Toute requête qui modifie exige en plus l'en-tête X-Lezard (bloque les
// envois intersites). Après 5 échecs en 5 minutes, la connexion est suspendue.

const (
	sessionCookie = "lezard_session"
	sessionTTL    = 12 * time.Hour
)

type adminAuth struct {
	passHash [32]byte
	enabled  bool
	key      []byte
	now      func() time.Time

	mu       sync.Mutex
	failures []time.Time
}

func newAdminAuth(password string) *adminAuth {
	a := &adminAuth{enabled: password != "", now: time.Now, key: make([]byte, 32)}
	a.passHash = sha256.Sum256([]byte(password))
	if _, err := rand.Read(a.key); err != nil {
		panic(err)
	}
	return a
}

func (a *adminAuth) sign(exp int64) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func (a *adminAuth) valid(r *http.Request) bool {
	if !a.enabled {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	expS, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || a.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.sign(exp)))
}

// reserve compte un essai de connexion avant de tester le mot de passe : une
// rafale de requêtes parallèles ne peut donc pas dépasser la limite. Elle renvoie
// false si 5 essais ont déjà eu lieu ces 5 dernières minutes.
func (a *adminAuth) reserve() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := a.now().Add(-5 * time.Minute)
	keep := a.failures[:0]
	for _, t := range a.failures {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.failures = keep
	if len(a.failures) >= 5 {
		return false
	}
	a.failures = append(a.failures, a.now())
	return true
}

// succeeded efface les essais comptés : une connexion réussie remet le compteur à zéro.
func (a *adminAuth) succeeded() {
	a.mu.Lock()
	a.failures = nil
	a.mu.Unlock()
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "requête illisible")
		return false
	}
	return true
}

// admin enveloppe un handler qui exige une session (et X-Lezard pour les écritures).
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-Lezard") != "1" {
			writeError(w, http.StatusForbidden, "en-tête X-Lezard manquant")
			return
		}
		if !s.auth.valid(r) {
			writeError(w, http.StatusUnauthorized, "connexion requise")
			return
		}
		h(w, r)
	}
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"admin": s.auth.enabled, "logged_in": s.auth.valid(r)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Lezard") != "1" {
		writeError(w, http.StatusForbidden, "en-tête X-Lezard manquant")
		return
	}
	if !s.auth.enabled {
		writeError(w, http.StatusForbidden, "administration désactivée : LEZARD_ADMIN_PASSWORD n'est pas défini")
		return
	}
	if !s.auth.reserve() {
		writeError(w, http.StatusTooManyRequests, "trop d'essais : réessaie dans quelques minutes")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sum := sha256.Sum256([]byte(body.Password))
	if subtle.ConstantTimeCompare(sum[:], s.auth.passHash[:]) != 1 {
		s.refusedLogin.logf(s.now(), "connexion admin refusée depuis %s", r.RemoteAddr)
		time.Sleep(400 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "mot de passe incorrect")
		return
	}
	s.auth.succeeded()
	exp := s.auth.now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.auth.sign(exp), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(sessionTTL / time.Second),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// ---- réglages ----

type hostSettingOut struct {
	Host  string   `json:"host"`
	Color int      `json:"color"`
	Name  string   `json:"name"`
	Warn  *float64 `json:"warn"`
	Crit  *float64 `json:"crit"`
	Seen  bool     `json:"seen"`
}

type telegramOut struct {
	Enabled   bool   `json:"enabled"`
	HasToken  bool   `json:"has_token"`
	TokenHint string `json:"token_hint"`
	Bot       string `json:"bot"`
}

type settingsOut struct {
	Telegram   telegramOut      `json:"telegram"`
	Recipients []Recipient      `json:"recipients"`
	Rules      Rules            `json:"rules"`
	Hosts      []hostSettingOut `json:"hosts"`
}

func (s *Server) settingsView() settingsOut {
	s.assignColors()
	set := s.settings.Get()
	out := settingsOut{
		Telegram:   telegramOut{Enabled: set.Telegram.Enabled, HasToken: set.Telegram.Token != "", Bot: set.Telegram.Bot},
		Recipients: set.Recipients,
		Rules:      set.Rules,
	}
	if t := set.Telegram.Token; len(t) > 4 {
		out.Telegram.TokenHint = "…" + t[len(t)-4:]
	}
	seen := map[string]bool{}
	for _, l := range s.store.LatestAll(s.now().Unix() - int64(s.cfg.Forget/time.Second)) {
		seen[l.Host] = true
	}
	names := map[string]bool{}
	for h := range seen {
		names[h] = true
	}
	for h := range set.Hosts {
		names[h] = true
	}
	for h := range names {
		hs := set.Hosts[h]
		out.Hosts = append(out.Hosts, hostSettingOut{Host: h, Color: hs.Color, Name: hs.Name, Warn: hs.Warn, Crit: hs.Crit, Seen: seen[h]})
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Host < out.Hosts[j].Host })
	if out.Hosts == nil {
		out.Hosts = []hostSettingOut{}
	}
	return out
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

type settingsIn struct {
	Telegram struct {
		Enabled    bool   `json:"enabled"`
		Token      string `json:"token"`       // vide = garder l'actuel
		ClearToken bool   `json:"clear_token"` // oublier le jeton
	} `json:"telegram"`
	Recipients []Recipient      `json:"recipients"`
	Rules      Rules            `json:"rules"`
	Hosts      []hostSettingOut `json:"hosts"`
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in settingsIn
	if !readJSON(w, r, &in) {
		return
	}
	cur := s.settings.Get()
	next := &Settings{
		Telegram:   cur.Telegram,
		Recipients: in.Recipients,
		Rules:      in.Rules,
		Hosts:      map[string]HostSetting{},
	}
	if next.Recipients == nil {
		next.Recipients = []Recipient{}
	}
	next.Telegram.Enabled = in.Telegram.Enabled
	if in.Telegram.ClearToken {
		next.Telegram.Token, next.Telegram.Bot, next.Telegram.Enabled = "", "", false
	}
	if tok := strings.TrimSpace(in.Telegram.Token); tok != "" {
		if !botRe.MatchString(tok) {
			writeError(w, http.StatusBadRequest, "jeton du bot illisible : format attendu 123456789:ABC…")
			return
		}
		bot, err := telegramBotName(tok)
		if err != nil {
			writeError(w, http.StatusBadRequest, "jeton non enregistré : "+err.Error())
			return
		}
		next.Telegram.Token, next.Telegram.Bot = tok, bot
	}
	if next.Telegram.Enabled && next.Telegram.Token == "" {
		writeError(w, http.StatusBadRequest, "enregistre d'abord le jeton du bot pour activer les alertes")
		return
	}
	for _, h := range in.Hosts {
		h.Host = strings.ToLower(strings.TrimSpace(h.Host))
		if _, dup := next.Hosts[h.Host]; dup {
			writeError(w, http.StatusBadRequest, "machine en double : "+h.Host)
			return
		}
		next.Hosts[h.Host] = HostSetting{Color: h.Color, Name: h.Name, Warn: h.Warn, Crit: h.Crit}
	}
	if err := s.settings.Set(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChatID string `json:"chat_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	set := s.settings.Get()
	if set.Telegram.Token == "" {
		writeError(w, http.StatusBadRequest, "aucun jeton de bot enregistré")
		return
	}
	in.ChatID = strings.TrimSpace(in.ChatID)
	if !chatRe.MatchString(in.ChatID) {
		writeError(w, http.StatusBadRequest, "identifiant de chat illisible")
		return
	}
	if err := telegramSend(set.Telegram.Token, in.ChatID, "✅ Lézard : message de test, ce chat recevra les alertes de température."); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTelegramChats(w http.ResponseWriter, _ *http.Request) {
	set := s.settings.Get()
	if set.Telegram.Token == "" {
		writeError(w, http.StatusBadRequest, "aucun jeton de bot enregistré")
		return
	}
	chats, err := telegramChats(set.Telegram.Token)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}
