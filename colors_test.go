package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestColorsStableAndDistinct(t *testing.T) {
	s, st, ck := newTestServer(t)
	for _, h := range []string{"alpha", "beta", "gamma"} {
		_ = st.AddAt(h, "cpu", 40, ck.t.Add(-10*time.Second))
	}
	var out struct {
		Hosts []struct {
			Host  string `json:"host"`
			Color int    `json:"color"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(get(s, "/api/hosts").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	first := map[string]int{}
	for _, h := range out.Hosts {
		if h.Color < 1 || h.Color > paletteSize || seen[h.Color] {
			t.Fatalf("couleur invalide ou en double : %+v", out.Hosts)
		}
		seen[h.Color] = true
		first[h.Host] = h.Color
	}
	// une nouvelle machine ne repeint pas les autres
	_ = st.AddAt("aaa", "cpu", 40, ck.t.Add(-5*time.Second))
	_ = json.Unmarshal(get(s, "/api/hosts").Body.Bytes(), &out)
	for _, h := range out.Hosts {
		if c, ok := first[h.Host]; ok && c != h.Color {
			t.Fatalf("%s a changé de couleur : %d -> %d", h.Host, c, h.Color)
		}
	}
}

func TestHistoryAll(t *testing.T) {
	s, st, ck := newTestServer(t)
	for i := 0; i < 5; i++ {
		ts := ck.t.Add(-time.Duration(i) * time.Minute)
		_ = st.AddAt("a", "cpu", 40+float64(i), ts)
		_ = st.AddAt("a", "nvme", 30, ts)
		_ = st.AddAt("b", "cpu", 50, ts)
	}
	var out struct {
		Step  int64 `json:"step"`
		Hosts []struct {
			Host    string `json:"host"`
			Color   int    `json:"color"`
			Sensors []struct {
				Sensor string       `json:"sensor"`
				Rows   [][5]float64 `json:"rows"`
			} `json:"sensors"`
		} `json:"hosts"`
	}
	w := get(s, "/api/history/all?range=1h")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Hosts) != 2 || out.Hosts[0].Host != "a" || len(out.Hosts[0].Sensors) != 2 || out.Hosts[0].Sensors[0].Sensor != "cpu" {
		t.Fatalf("réponse inattendue : %s", w.Body.String())
	}
	if len(out.Hosts[1].Sensors[0].Rows) == 0 || out.Hosts[0].Color == out.Hosts[1].Color {
		t.Fatalf("lignes vides ou couleurs identiques : %s", w.Body.String())
	}
}

func TestColorSettingValidation(t *testing.T) {
	st := &Settings{Rules: Rules{Warn: 70, Crit: 80, Hyst: 2}, Hosts: map[string]HostSetting{"x": {Color: 13}}}
	if err := st.validate(); err == nil {
		t.Fatal("couleur 13 acceptée")
	}
}
