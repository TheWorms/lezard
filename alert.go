package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Alertes Telegram centralisées. À chaque passage (20 s), Lézard compare chaque
// machine à ses seuils ; un changement d'état confirmé devient un événement
// (critical, warning, offline, recovery, reminder). Chaque destinataire reçoit,
// en un seul message, les événements qui le concernent (types, machines, plage
// calme). L'état notifié est conservé dans alerts.json ; les messages non
// délivrés sont retentés pendant une heure.

type hostAlert struct {
	Level  string    `json:"level"`
	Since  time.Time `json:"since"`
	SentAt time.Time `json:"sent_at"`
	// en mémoire seulement : changement en attente de confirmation
	pending      string
	pendingSince time.Time
}

type alertEvent struct {
	host string
	kind string
	to   string
	line string
}

// outMsg est un message en attente d'envoi ; la file est gardée dans outbox.json
// pour survivre à un redémarrage pendant une panne de Telegram.
type outMsg struct {
	Chat    string    `json:"chat"`
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
}

// Sender envoie un texte à un chat avec le jeton de bot donné.
type Sender func(token, chat, text string) error

type Alerter struct {
	cfg      *Config
	settings *SettingsStore
	store    *Store
	send     Sender
	now      func() time.Time
	started  time.Time
	path     string
	outPath  string
	ctx      context.Context // annulé à l'arrêt : l'envoi en cours s'interrompt entre deux messages
	onErr    func()          // signale un échec d'écriture disque (pour /api/health)

	// sendMu sérialise les passages complets (évaluation + envoi) ; mu protège
	// l'état et les compteurs et n'est jamais tenu pendant un appel réseau, pour
	// que /api/health réponde même quand Telegram est lent ou injoignable.
	sendMu sync.Mutex

	mu        sync.Mutex
	state     map[string]*hostAlert
	outbox    []outMsg
	errors    int
	lastError string
	sent      int
}

func NewAlerter(cfg *Config, settings *SettingsStore, store *Store, send Sender) *Alerter {
	a := &Alerter{cfg: cfg, settings: settings, store: store, send: send, now: time.Now, state: map[string]*hostAlert{}}
	a.started = a.now()
	a.ctx = context.Background()
	a.onErr = store.NoteWriteError
	a.path = filepath.Join(store.Dir(), "alerts.json")
	a.outPath = filepath.Join(store.Dir(), "outbox.json")
	if b, err := os.ReadFile(a.outPath); err == nil {
		if err := json.Unmarshal(b, &a.outbox); err != nil {
			log.Printf("outbox.json illisible, file d'envoi vidée : %v", err)
			a.outbox = nil
		}
	}
	if b, err := os.ReadFile(a.path); err == nil {
		if err := json.Unmarshal(b, &a.state); err != nil {
			log.Printf("alerts.json illisible, état des alertes repart de zéro : %v", err)
			a.state = map[string]*hostAlert{}
		}
	}
	return a
}

type hostNow struct {
	host    string
	temp    float64 // capteur actif le plus chaud
	sensor  string
	age     int64
	offline bool
}

func (a *Alerter) snapshot(now time.Time) map[string]hostNow {
	nowT := now.Unix()
	stale := int64(a.cfg.Stale / time.Second)
	out := map[string]hostNow{}
	for _, l := range a.store.LatestAll(nowT - int64(a.cfg.Forget/time.Second)) {
		h, ok := out[l.Host]
		age := nowT - l.T
		if !ok {
			h = hostNow{host: l.Host, temp: math.Inf(-1), age: age}
		}
		if age < h.age {
			h.age = age
		}
		if age <= stale && l.C > h.temp {
			h.temp, h.sensor = l.C, l.Sensor
		}
		out[l.Host] = h
	}
	for k, h := range out {
		h.offline = h.age > stale
		out[k] = h
	}
	return out
}

// candidate applique l'hystérésis : pour redescendre d'un niveau, la
// température doit passer sous le seuil moins l'hystérésis.
func candidate(set *Settings, h hostNow, prev string) string {
	if h.offline {
		return statusOffline
	}
	warn, crit := set.thresholds(h.host)
	c := statusFor(h.temp, warn, crit)
	if prev == statusCritical && c != statusCritical && h.temp >= crit-set.Rules.Hyst {
		c = statusCritical
	}
	if (prev == statusWarning || prev == statusCritical) && c == statusOK && h.temp >= warn-set.Rules.Hyst {
		c = statusWarning
	}
	return c
}

func eventKind(to string) string {
	switch to {
	case statusCritical:
		return evCritical
	case statusWarning:
		return evWarning
	case statusOffline:
		return evOffline
	default:
		return evRecovery
	}
}

// SetContext relie l'arrêt du programme à l'envoi des alertes.
func (a *Alerter) SetContext(ctx context.Context) { a.ctx = ctx }

// Check évalue toutes les machines, prépare les messages et vide la file d'envoi.
// Aucun verrou n'est tenu pendant l'écriture disque ni pendant les appels réseau.
func (a *Alerter) Check() {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	set := a.settings.Get()
	now := a.now()

	var stateJSON, outJSON []byte
	var pending []outMsg
	func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.evaluateLocked(set, now) {
			stateJSON = a.marshalStateLocked()
			outJSON = a.marshalOutboxLocked()
		}
		pending = a.outbox
		a.outbox = nil
	}()
	a.persist(a.path, stateJSON)
	a.persist(a.outPath, outJSON) // les messages sont sur disque avant leur envoi

	keep, sent, errs, lastErr := a.deliver(set, now, pending)

	outJSON = nil
	func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		changed := len(keep) != len(pending)
		a.outbox = append(keep, a.outbox...)
		a.sent += sent
		a.errors += errs
		if lastErr != "" {
			a.lastError = lastErr
		}
		if changed {
			outJSON = a.marshalOutboxLocked()
		}
	}()
	a.persist(a.outPath, outJSON)
}

func (a *Alerter) persist(path string, b []byte) {
	if b == nil {
		return
	}
	if err := writeFileAtomic(path, b, 0o600); err != nil {
		log.Printf("%s : %v", filepath.Base(path), err)
		if a.onErr != nil {
			a.onErr()
		}
	}
}

func (a *Alerter) marshalStateLocked() []byte {
	b, err := json.Marshal(a.state)
	if err != nil {
		return nil
	}
	return b
}

func (a *Alerter) marshalOutboxLocked() []byte {
	out := a.outbox
	if out == nil {
		out = []outMsg{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// maxMsgLen : longueur maximale d'un message (Telegram refuse au-delà de 4096 caractères).
const maxMsgLen = 3500

// splitLines regroupe des lignes en messages de maxMsgLen octets au plus.
func splitLines(lines []string) []string {
	var out []string
	var b strings.Builder
	for _, l := range lines {
		if len(l) > maxMsgLen {
			l = strings.ToValidUTF8(l[:maxMsgLen], "")
		}
		if b.Len() > 0 && b.Len()+1+len(l) > maxMsgLen {
			out = append(out, b.String())
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// evaluateLocked compare chaque machine à ses seuils et ajoute les messages à la file.
// Elle renvoie true si l'état notifié a changé (à écrire sur disque).
func (a *Alerter) evaluateLocked(set *Settings, now time.Time) bool {
	snap := a.snapshot(now)
	inGrace := now.Sub(a.started) < a.cfg.Stale+30*time.Second

	var events []alertEvent
	for name, h := range snap {
		st := a.state[name]
		if st == nil {
			// machine découverte : hors ligne d'emblée = silencieux, sinon on part de OK
			lvl := statusOK
			if h.offline {
				lvl = statusOffline
			}
			st = &hostAlert{Level: lvl, Since: now}
			a.state[name] = st
		}
		c := candidate(set, h, st.Level)
		if c == statusOffline && inGrace {
			// au démarrage, les machines n'ont pas encore eu le temps de pousser
			c = st.Level
		}
		if c == st.Level {
			st.pending = ""
			if st.Level == statusCritical && set.repeat() > 0 && now.Sub(st.SentAt) >= set.repeat() {
				events = append(events, alertEvent{name, evReminder, c, alertLine(set, h, c, st.Level, true, now.Sub(st.Since))})
			}
			continue
		}
		if st.pending != c {
			st.pending, st.pendingSince = c, now
		}
		// Critique et Hors ligne partent tout de suite (Hors ligne a déjà son délai LEZARD_STALE)
		if c != statusCritical && c != statusOffline && now.Sub(st.pendingSince) < set.delay() {
			continue
		}
		events = append(events, alertEvent{name, eventKind(c), c, alertLine(set, h, c, st.Level, false, 0)})
	}
	for name := range a.state {
		if _, ok := snap[name]; !ok {
			delete(a.state, name) // machine oubliée (muette depuis LEZARD_FORGET)
		}
	}

	if len(events) > 0 {
		for _, e := range events {
			st := a.state[e.host]
			if st.Level != e.to {
				st.Since = now
			}
			st.Level, st.SentAt, st.pending = e.to, now, ""
		}
		sort.Slice(events, func(i, j int) bool {
			si, sj := severity(events[i].to), severity(events[j].to)
			if si != sj {
				return si > sj
			}
			return events[i].host < events[j].host
		})
		if set.Telegram.Enabled && set.Telegram.Token != "" {
			for _, rc := range set.Recipients {
				var lines []string
				for _, e := range events {
					if rc.wants(e.kind, e.host, now) {
						lines = append(lines, e.line)
					}
				}
				for _, text := range splitLines(lines) {
					a.outbox = append(a.outbox, outMsg{Chat: rc.ChatID, Text: text, Created: now})
				}
			}
		}
	}
	return len(events) > 0
}

// deliver envoie les messages en attente, sans tenir a.mu. Elle renvoie ceux à
// retenter au prochain passage. Un refus définitif (chat introuvable, bot
// bloqué…) abandonne le message ; une panne réseau arrête le passage pour ne pas
// enchaîner les délais d'attente.
func (a *Alerter) deliver(set *Settings, now time.Time, msgs []outMsg) (keep []outMsg, sent, errs int, lastErr string) {
	if len(msgs) == 0 || a.send == nil {
		return msgs, 0, 0, ""
	}
	if !set.Telegram.Enabled || set.Telegram.Token == "" {
		return nil, 0, 0, ""
	}
	down := false
	for _, m := range msgs {
		if now.Sub(m.Created) > time.Hour {
			log.Printf("alerte abandonnée après 1 h d'échecs (chat %s)", m.Chat)
			continue
		}
		if down || a.ctx.Err() != nil { // panne en cours ou arrêt demandé : on garde pour plus tard
			keep = append(keep, m)
			continue
		}
		err := a.send(set.Telegram.Token, m.Chat, m.Text)
		if err == nil {
			sent++
			continue
		}
		errs++
		lastErr = err.Error()
		if isPermanent(err) {
			log.Printf("alerte abandonnée pour le chat %s (refus définitif) : %v", m.Chat, err)
			continue
		}
		log.Printf("alerte non envoyée au chat %s (nouvel essai au prochain passage) : %v", m.Chat, err)
		keep = append(keep, m)
		if isNetwork(err) {
			down = true
		}
	}
	return keep, sent, errs, lastErr
}

func alertLine(set *Settings, h hostNow, to, from string, reminder bool, dur time.Duration) string {
	name := set.displayName(h.host)
	warn, crit := set.thresholds(h.host)
	t := strings.Replace(fmt.Sprintf("%.1f", h.temp), ".", ",", 1)
	sensor := ""
	if h.sensor != "" && h.sensor != "cpu" {
		sensor = " (" + h.sensor + ")"
	}
	switch {
	case reminder:
		return fmt.Sprintf("🔴 Rappel : %s toujours critique depuis %s, %s °C%s", name, humanDur(dur), t, sensor)
	case to == statusCritical:
		return fmt.Sprintf("🔴 %s : CRITIQUE, %s °C%s (seuil %g °C)", name, t, sensor, crit)
	case to == statusWarning:
		return fmt.Sprintf("🟠 %s : attention, %s °C%s (seuil %g °C)", name, t, sensor, warn)
	case to == statusOffline:
		return fmt.Sprintf("⚫ %s : hors ligne, aucun relevé depuis %s", name, humanDur(time.Duration(h.age)*time.Second))
	case from == statusOffline:
		return fmt.Sprintf("🟢 %s : de nouveau en ligne, %s °C%s", name, t, sensor)
	default:
		return fmt.Sprintf("🟢 %s : retour à la normale, %s °C%s", name, t, sensor)
	}
}

func humanDur(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "moins d'une minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 48*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d h", m/60)
		}
		return fmt.Sprintf("%d h %02d", m/60, m%60)
	default:
		return fmt.Sprintf("%d j", m/1440)
	}
}

// Stats pour /api/health.
func (a *Alerter) Stats() map[string]any {
	set := a.settings.Get()
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"enabled":    set.Telegram.Enabled && set.Telegram.Token != "",
		"recipients": len(set.Recipients),
		"sent":       a.sent,
		"errors":     a.errors,
		"pending":    len(a.outbox),
	}
}
