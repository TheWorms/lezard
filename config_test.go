package main

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Warn != 70 || c.Crit != 80 || c.Stale != 3*time.Minute || c.Listen != ":3070" || c.RawDays != 3 {
		t.Fatalf("défauts inattendus : %+v", c)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"warn>=crit":  {"LEZARD_WARN": "90", "LEZARD_CRIT": "80"},
		"stale court": {"LEZARD_STALE": "5s"},
		"raw days":    {"LEZARD_RAW_DAYS": "99"},
		"seuils":      {"LEZARD_THRESHOLDS": "pi=abc"},
		"noms":        {"LEZARD_NAMES": "=x"},
	}
	for name, m := range cases {
		if _, err := loadConfig(env(m)); err == nil {
			t.Errorf("%s : erreur attendue", name)
		}
	}
}

func TestThresholdsAndNames(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"LEZARD_THRESHOLDS": "serveur=80/88, pi4=65.5/75",
		"LEZARD_NAMES":      "media=Média",
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := seedSettings(c)
	if w, cr := s.thresholds("serveur"); w != 80 || cr != 88 {
		t.Errorf("serveur : %v/%v", w, cr)
	}
	if w, cr := s.thresholds("autre"); w != 70 || cr != 80 {
		t.Errorf("défaut : %v/%v", w, cr)
	}
	if s.displayName("media") != "Média" || s.displayName("cuisine") != "Cuisine" {
		t.Error("noms affichés incorrects")
	}
	if _, err := loadConfig(env(map[string]string{"LEZARD_ADMIN_PASSWORD": "court"})); err == nil {
		t.Error("mot de passe admin trop court accepté")
	}
}

func TestStatusFor(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{{69.9, statusOK}, {70, statusWarning}, {79.9, statusWarning}, {80, statusCritical}} {
		if got := statusFor(tc.v, 70, 80); got != tc.want {
			t.Errorf("%v : %s, attendu %s", tc.v, got, tc.want)
		}
	}
	if !strings.Contains(strings.Join([]string{statusOffline}, ""), "off") || severity(statusCritical) <= severity(statusWarning) {
		t.Error("ordre de gravité")
	}
}
