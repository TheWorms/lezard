package main

import (
	"context"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// Données fictives pour prévisualiser l'interface : lezard -demo. Rien n'est écrit
// hors d'un dossier temporaire supprimé à l'arrêt.

type demoHost struct {
	host    string
	base    float64
	amp     float64
	noise   float64
	offline bool // plus aucun relevé depuis deux jours
}

var demoFleet = []demoHost{
	{"serveur", 56, 4, 0.6, false},
	{"dns", 52, 3, 0.5, false},
	{"nas", 49, 3, 0.5, false},
	{"atelier", 55, 4, 0.6, false},
	{"domotique", 60, 7, 0.9, false},
	{"cuisine", 46, 3, 0.4, false},
	{"bureau", 44, 3, 0.4, false},
	{"garage", 41, 2, 0.3, false},
	{"media", 58, 5, 0.7, false},
	{"salon", 54, 5, 0.7, false},
	{"vieux-pi", 40, 2, 0.3, true},
}

func demoNames() map[string]string {
	return map[string]string{"serveur": "Serveur", "nas": "NAS", "dns": "DNS", "media": "Média", "vieux-pi": "Vieux Pi"}
}

func demoThresholds() map[string][2]float64 {
	return map[string][2]float64{"serveur": {80, 88}}
}

// demoTemp calcule la température fictive de h à l'instant t.
func demoTemp(h demoHost, t, now time.Time, r *rand.Rand, drift *float64) float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60
	v := h.base + h.amp*math.Sin(2*math.Pi*(hour-9)/24)
	*drift = *drift*0.97 + r.NormFloat64()*h.noise*0.4
	v += *drift
	if h.host == "serveur" && t.Weekday() == time.Sunday && t.Hour() == 1 {
		v += 26
	}
	age := now.Sub(t)
	if h.host == "domotique" && age < 70*time.Minute {
		v += 11
	}
	if h.host == "media" && age < 15*time.Minute {
		v += 24
	}
	return v
}

// fillDemo remplit 7 jours de relevés à la minute en rejouant l'horloge du store.
func fillDemo(s *Store) {
	real := s.opt.Now
	now := real()
	cur := now
	s.mu.Lock()
	s.opt.Now = func() time.Time { return cur }
	s.mu.Unlock()
	r := rand.New(rand.NewSource(42))
	drifts := make([]float64, len(demoFleet))
	start := now.Add(-7 * 24 * time.Hour).Truncate(time.Minute)
	for t := start; !t.After(now); t = t.Add(time.Minute) {
		cur = t
		for i, h := range demoFleet {
			if h.offline && now.Sub(t) < 48*time.Hour {
				continue
			}
			_ = s.AddAt(h.host, "cpu", demoTemp(h, t, now, r, &drifts[i]), t)
		}
		if t.Minute()%10 == 0 {
			s.Maintain()
		}
	}
	cur = now
	s.mu.Lock()
	s.opt.Now = real
	s.mu.Unlock()
	s.Maintain()
}

// liveDemo ajoute un relevé toutes les 5 s pour voir l'interface vivre.
func liveDemo(ctx context.Context, s *Store) {
	r := rand.New(rand.NewSource(7))
	last := map[string]float64{}
	for _, h := range demoFleet {
		hist := s.History(h.host, "cpu", time.Now().Add(-10*time.Minute), 0)
		if len(hist) > 0 {
			last[h.host] = hist[len(hist)-1].Avg
		} else {
			last[h.host] = h.base
		}
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, h := range demoFleet {
				if h.offline {
					continue
				}
				target := h.base
				switch h.host {
				case "domotique":
					target = 72
				case "media":
					target = 82
				}
				last[h.host] += (target-last[h.host])*0.05 + r.NormFloat64()*h.noise*0.3
				_ = s.Add(h.host, "cpu", last[h.host])
			}
		}
	}
}

// demoTelegram remplace l'API Telegram par une imitation locale en mode démo,
// pour essayer la page Réglages sans vrai bot. Tout jeton au bon format est accepté.
func demoTelegram() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"lezard_demo_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"message":{"chat":{"id":123456789,"type":"private","first_name":"Camille","username":"camille_demo"}}},{"channel_post":{"chat":{"id":-1001234567890,"type":"channel","title":"Alertes maison"}}}]}`))
		default:
			_ = r.ParseForm()
			log.Printf("démo Telegram, message pour %s :\n%s", r.PostForm.Get("chat_id"), r.PostForm.Get("text"))
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	})
	go func() { _ = http.Serve(ln, mux) }()
	telegramAPI = "http://" + ln.Addr().String()
}
