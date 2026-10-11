package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	statusOK       = "ok"
	statusWarning  = "warning"
	statusCritical = "critical"
	statusOffline  = "offline"
)

// Config regroupe les réglages, tous lus dans l'environnement.
type Config struct {
	Listen    string
	DataDir   string
	Token     string
	Warn      float64
	Crit      float64
	PerHost   map[string][2]float64
	Names     map[string]string
	Stale     time.Duration
	Forget    time.Duration
	RawDays   int
	MaxSeries int
	DemoMode  bool

	AdminPassword string
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// loadConfig lit la configuration depuis getenv (os.Getenv en production).
func loadConfig(getenv func(string) string) (*Config, error) {
	c := &Config{
		Listen:    ":3070",
		DataDir:   "/data",
		Warn:      70,
		Crit:      80,
		Stale:     3 * time.Minute,
		Forget:    30 * 24 * time.Hour,
		RawDays:   3,
		MaxSeries: 100,
	}
	if v := getenv("LEZARD_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("LEZARD_DATA"); v != "" {
		c.DataDir = v
	}
	c.Token = getenv("LEZARD_TOKEN")
	var err error
	if v := getenv("LEZARD_WARN"); v != "" {
		if c.Warn, err = parseTemp("LEZARD_WARN", v); err != nil {
			return nil, err
		}
	}
	if v := getenv("LEZARD_CRIT"); v != "" {
		if c.Crit, err = parseTemp("LEZARD_CRIT", v); err != nil {
			return nil, err
		}
	}
	if c.Warn >= c.Crit {
		return nil, fmt.Errorf("LEZARD_WARN (%g) doit être inférieur à LEZARD_CRIT (%g)", c.Warn, c.Crit)
	}
	if v := getenv("LEZARD_STALE"); v != "" {
		if c.Stale, err = time.ParseDuration(v); err != nil || c.Stale < 30*time.Second {
			return nil, fmt.Errorf("LEZARD_STALE invalide (%q) : durée d'au moins 30s attendue, ex. 3m", v)
		}
	}
	if v := getenv("LEZARD_FORGET"); v != "" {
		if c.Forget, err = time.ParseDuration(v); err != nil || c.Forget < time.Hour {
			return nil, fmt.Errorf("LEZARD_FORGET invalide (%q) : durée d'au moins 1h attendue, ex. 720h", v)
		}
	}
	if v := getenv("LEZARD_RAW_DAYS"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 30 {
			return nil, fmt.Errorf("LEZARD_RAW_DAYS invalide (%q) : entier de 1 à 30 attendu", v)
		}
		c.RawDays = n
	}
	if v := getenv("LEZARD_MAX_SERIES"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 1000 {
			return nil, fmt.Errorf("LEZARD_MAX_SERIES invalide (%q) : entier de 1 à 1000 attendu", v)
		}
		c.MaxSeries = n
	}
	if c.PerHost, err = parseThresholds(getenv("LEZARD_THRESHOLDS")); err != nil {
		return nil, err
	}
	if c.Names, err = parseNames(getenv("LEZARD_NAMES")); err != nil {
		return nil, err
	}

	c.AdminPassword = getenv("LEZARD_ADMIN_PASSWORD")
	if c.AdminPassword != "" && len([]rune(c.AdminPassword)) < 10 {
		return nil, fmt.Errorf("LEZARD_ADMIN_PASSWORD trop court : 10 caractères minimum")
	}
	return c, nil
}

func parseTemp(name, v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), ",", "."), 64)
	if err != nil || f < -50 || f > 150 {
		return 0, fmt.Errorf("%s invalide (%q) : température en °C attendue", name, v)
	}
	return f, nil
}

// parseThresholds lit "hote=warn/crit,hote2=warn/crit".
func parseThresholds(s string) (map[string][2]float64, error) {
	out := map[string][2]float64{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		host, vals, ok := strings.Cut(part, "=")
		w, c, ok2 := strings.Cut(vals, "/")
		host = strings.ToLower(strings.TrimSpace(host))
		if !ok || !ok2 || !nameRe.MatchString(host) {
			return nil, fmt.Errorf("LEZARD_THRESHOLDS : %q illisible, format attendu hote=warn/crit", part)
		}
		wf, err := parseTemp("LEZARD_THRESHOLDS", w)
		if err != nil {
			return nil, err
		}
		cf, err := parseTemp("LEZARD_THRESHOLDS", c)
		if err != nil {
			return nil, err
		}
		if wf >= cf {
			return nil, fmt.Errorf("LEZARD_THRESHOLDS : %s, le seuil d'attention (%g) doit être inférieur au seuil critique (%g)", host, wf, cf)
		}
		out[host] = [2]float64{wf, cf}
	}
	return out, nil
}

// parseNames lit "nas=NAS,media=Média".
func parseNames(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		host, name, ok := strings.Cut(part, "=")
		host, name = strings.ToLower(strings.TrimSpace(host)), strings.TrimSpace(name)
		if !ok || !nameRe.MatchString(host) || name == "" || len([]rune(name)) > 40 {
			return nil, fmt.Errorf("LEZARD_NAMES : %q illisible, format attendu hote=Nom", part)
		}
		out[host] = name
	}
	return out, nil
}

func statusFor(temp, warn, crit float64) string {
	switch {
	case temp >= crit:
		return statusCritical
	case temp >= warn:
		return statusWarning
	default:
		return statusOK
	}
}

// severity classe les statuts pour choisir le pire.
func severity(st string) int {
	switch st {
	case statusCritical:
		return 3
	case statusWarning:
		return 2
	case statusOffline:
		return 1
	default:
		return 0
	}
}

func getenvOS(k string) string { return os.Getenv(k) }
