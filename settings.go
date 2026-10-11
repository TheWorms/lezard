package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Réglages modifiables depuis l'interface, conservés dans /data/settings.json
// (droits 600). Au premier démarrage, ils sont initialisés depuis l'environnement.

const (
	evCritical = "critical"
	evWarning  = "warning"
	evOffline  = "offline"
	evRecovery = "recovery"
	evReminder = "reminder"
)

var allEvents = []string{evCritical, evWarning, evOffline, evRecovery, evReminder}

var (
	chatRe  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,64})$`)
	botRe   = regexp.MustCompile(`^[0-9]{5,15}:[A-Za-z0-9_-]{30,64}$`)
	clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	idRe    = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

type Recipient struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ChatID        string   `json:"chat_id"`
	Enabled       bool     `json:"enabled"`
	Events        []string `json:"events"`
	Hosts         []string `json:"hosts"` // vide = toutes les machines
	QuietFrom     string   `json:"quiet_from"`
	QuietTo       string   `json:"quiet_to"`
	QuietCritical bool     `json:"quiet_critical"` // les critiques passent pendant la plage calme
}

type Rules struct {
	Warn      float64 `json:"warn"`
	Crit      float64 `json:"crit"`
	DelayMin  float64 `json:"delay_min"`
	RepeatMin float64 `json:"repeat_min"`
	Hyst      float64 `json:"hyst"`
}

// paletteSize : nombre de couleurs de machine définies dans app.css (--h1 à --h12).
const paletteSize = 12

type HostSetting struct {
	Color int      `json:"color,omitempty"` // 1 à paletteSize, 0 = pas encore attribuée
	Name  string   `json:"name,omitempty"`
	Warn  *float64 `json:"warn,omitempty"`
	Crit  *float64 `json:"crit,omitempty"`
}

type TelegramSettings struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
	Bot     string `json:"bot"` // @nom du bot, relevé à l'enregistrement
}

type Settings struct {
	Telegram   TelegramSettings       `json:"telegram"`
	Recipients []Recipient            `json:"recipients"`
	Rules      Rules                  `json:"rules"`
	Hosts      map[string]HostSetting `json:"hosts"`
}

func (s *Settings) thresholds(host string) (warn, crit float64) {
	warn, crit = s.Rules.Warn, s.Rules.Crit
	if h, ok := s.Hosts[host]; ok && h.Warn != nil && h.Crit != nil {
		warn, crit = *h.Warn, *h.Crit
	}
	return
}

func (s *Settings) displayName(host string) string {
	if h, ok := s.Hosts[host]; ok && h.Name != "" {
		return h.Name
	}
	if host == "" {
		return host
	}
	r := []rune(host)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func (s *Settings) delay() time.Duration {
	return time.Duration(s.Rules.DelayMin * float64(time.Minute))
}

func (s *Settings) repeat() time.Duration {
	return time.Duration(s.Rules.RepeatMin * float64(time.Minute))
}

func (s *Settings) clone() *Settings {
	b, _ := json.Marshal(s)
	var c Settings
	_ = json.Unmarshal(b, &c)
	if c.Hosts == nil {
		c.Hosts = map[string]HostSetting{}
	}
	if c.Recipients == nil {
		c.Recipients = []Recipient{}
	}
	return &c
}

func seedSettings(cfg *Config) *Settings {
	s := &Settings{
		Recipients: []Recipient{},
		Rules:      Rules{Warn: cfg.Warn, Crit: cfg.Crit, DelayMin: 1, RepeatMin: 60, Hyst: 2},
		Hosts:      map[string]HostSetting{},
	}
	for h, t := range cfg.PerHost {
		w, c := t[0], t[1]
		hs := s.Hosts[h]
		hs.Warn, hs.Crit = &w, &c
		s.Hosts[h] = hs
	}
	for h, n := range cfg.Names {
		hs := s.Hosts[h]
		hs.Name = n
		s.Hosts[h] = hs
	}
	return s
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validate contrôle et normalise des réglages reçus de l'interface.
func (s *Settings) validate() error {
	r := &s.Rules
	for _, v := range []float64{r.Warn, r.Crit} {
		if v < -50 || v > 150 {
			return fmt.Errorf("seuils : valeurs de -50 à 150 °C attendues")
		}
	}
	if r.Warn >= r.Crit {
		return fmt.Errorf("le seuil d'attention doit être inférieur au seuil critique")
	}
	if r.DelayMin < 0 || r.DelayMin > 60 {
		return fmt.Errorf("délai de confirmation : 0 à 60 minutes")
	}
	if r.RepeatMin != 0 && (r.RepeatMin < 5 || r.RepeatMin > 1440) {
		return fmt.Errorf("rappel : 0 (jamais) ou 5 à 1440 minutes")
	}
	if r.Hyst < 0 || r.Hyst > 20 {
		return fmt.Errorf("hystérésis : 0 à 20 °C")
	}
	if s.Telegram.Token != "" && !botRe.MatchString(s.Telegram.Token) {
		return fmt.Errorf("jeton du bot illisible : format attendu 123456789:ABC…")
	}
	if len(s.Recipients) > 20 {
		return fmt.Errorf("20 destinataires au maximum")
	}
	seen := map[string]bool{}
	for i := range s.Recipients {
		rc := &s.Recipients[i]
		rc.Name = strings.TrimSpace(rc.Name)
		rc.ChatID = strings.TrimSpace(rc.ChatID)
		if rc.Name == "" || len([]rune(rc.Name)) > 40 {
			return fmt.Errorf("destinataire %d : nom de 1 à 40 caractères attendu", i+1)
		}
		if !chatRe.MatchString(rc.ChatID) {
			return fmt.Errorf("%s : identifiant de chat illisible (nombre, ex. 123456789 ou -100…, ou @canal)", rc.Name)
		}
		if !idRe.MatchString(rc.ID) || seen[rc.ID] {
			rc.ID = newID()
		}
		seen[rc.ID] = true
		evs := map[string]bool{}
		for _, e := range rc.Events {
			ok := false
			for _, k := range allEvents {
				if e == k {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("%s : événement inconnu %q", rc.Name, e)
			}
			evs[e] = true
		}
		rc.Events = rc.Events[:0]
		for _, k := range allEvents {
			if evs[k] {
				rc.Events = append(rc.Events, k)
			}
		}
		if rc.Hosts == nil {
			rc.Hosts = []string{}
		}
		for _, h := range rc.Hosts {
			if !nameRe.MatchString(h) {
				return fmt.Errorf("%s : machine %q invalide", rc.Name, h)
			}
		}
		sort.Strings(rc.Hosts)
		if (rc.QuietFrom == "") != (rc.QuietTo == "") {
			return fmt.Errorf("%s : plage calme incomplète (début et fin)", rc.Name)
		}
		if rc.QuietFrom != "" && (!clockRe.MatchString(rc.QuietFrom) || !clockRe.MatchString(rc.QuietTo) || rc.QuietFrom == rc.QuietTo) {
			return fmt.Errorf("%s : plage calme illisible (HH:MM, début différent de la fin)", rc.Name)
		}
	}
	if s.Hosts == nil {
		s.Hosts = map[string]HostSetting{}
	}
	for h, hs := range s.Hosts {
		if !nameRe.MatchString(h) {
			return fmt.Errorf("machine %q invalide", h)
		}
		hs.Name = strings.TrimSpace(hs.Name)
		if hs.Color < 0 || hs.Color > paletteSize {
			return fmt.Errorf("%s : couleur de 1 à %d attendue", h, paletteSize)
		}
		if len([]rune(hs.Name)) > 40 {
			return fmt.Errorf("%s : nom affiché de 40 caractères au maximum", h)
		}
		if (hs.Warn == nil) != (hs.Crit == nil) {
			return fmt.Errorf("%s : renseigner les deux seuils ou aucun", h)
		}
		if hs.Warn != nil {
			if *hs.Warn < -50 || *hs.Crit > 150 || *hs.Warn >= *hs.Crit {
				return fmt.Errorf("%s : seuils incohérents (attention < critique, de -50 à 150 °C)", h)
			}
		}
		if hs.Name == "" && hs.Warn == nil && hs.Color == 0 {
			delete(s.Hosts, h)
			continue
		}
		s.Hosts[h] = hs
	}
	return nil
}

// wants dit si ce destinataire doit recevoir l'événement kind de host à l'instant now.
func (rc *Recipient) wants(kind, host string, now time.Time) bool {
	if !rc.Enabled {
		return false
	}
	found := false
	for _, e := range rc.Events {
		if e == kind {
			found = true
		}
	}
	if !found {
		return false
	}
	if len(rc.Hosts) > 0 {
		found = false
		for _, h := range rc.Hosts {
			if h == host {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if rc.inQuiet(now) && !(kind == evCritical && rc.QuietCritical) {
		return false
	}
	return true
}

func (rc *Recipient) inQuiet(now time.Time) bool {
	if rc.QuietFrom == "" {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	from := clockMinutes(rc.QuietFrom)
	to := clockMinutes(rc.QuietTo)
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to // plage qui passe minuit
}

func clockMinutes(s string) int {
	var h, m int
	_, _ = fmt.Sscanf(s, "%d:%d", &h, &m)
	return h*60 + m
}

// SettingsStore garde les réglages en mémoire et sur disque.
type SettingsStore struct {
	mu   sync.RWMutex
	path string
	cur  *Settings
}

func OpenSettings(dir string, cfg *Config) (*SettingsStore, error) {
	ss := &SettingsStore{path: filepath.Join(dir, "settings.json")}
	b, err := os.ReadFile(ss.path)
	switch {
	case err == nil:
		var s Settings
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("settings.json illisible : %v", err)
		}
		if err := s.validate(); err != nil {
			return nil, fmt.Errorf("settings.json : %v", err)
		}
		ss.cur = &s
	case os.IsNotExist(err):
		ss.cur = seedSettings(cfg)
		if err := ss.save(ss.cur); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return ss, nil
}

// Get renvoie une copie des réglages courants.
func (ss *SettingsStore) Get() *Settings {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.cur.clone()
}

// Set valide puis enregistre de nouveaux réglages.
func (ss *SettingsStore) Set(s *Settings) error {
	if err := s.validate(); err != nil {
		return err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if err := ss.save(s); err != nil {
		return err
	}
	ss.cur = s.clone()
	return nil
}

// AssignColors donne une couleur stable aux machines qui n'en ont pas encore :
// la première libre, sinon la moins utilisée. L'attribution est enregistrée,
// donc une machine garde sa couleur même si d'autres apparaissent ou disparaissent.
func (ss *SettingsStore) AssignColors(hosts []string) {
	hosts = append([]string(nil), hosts...)
	sort.Strings(hosts)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	use := make([]int, paletteSize+1)
	for _, hs := range ss.cur.Hosts {
		if hs.Color > 0 {
			use[hs.Color]++
		}
	}
	changed := false
	for _, h := range hosts {
		hs := ss.cur.Hosts[h]
		if hs.Color > 0 {
			continue
		}
		best := 1
		for c := 2; c <= paletteSize; c++ {
			if use[c] < use[best] {
				best = c
			}
		}
		hs.Color = best
		use[best]++
		ss.cur.Hosts[h] = hs
		changed = true
	}
	if changed {
		if err := ss.save(ss.cur); err != nil {
			log.Printf("couleurs non enregistrées : %v", err)
		}
	}
}

func (ss *SettingsStore) save(s *Settings) error {
	if ss.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(ss.path, b, 0o600); err != nil {
		return err
	}
	log.Printf("réglages enregistrés")
	return nil
}
