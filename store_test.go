package main

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newClock() *clock { return &clock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)} }

func TestStoreAddAndHistory(t *testing.T) {
	ck := newClock()
	s, err := OpenStore(t.TempDir(), StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 60; i++ {
		at := ck.t.Add(time.Duration(i-60) * time.Minute)
		if err := s.AddAt("pi", "cpu", 50+float64(i%10), at); err != nil {
			t.Fatal(err)
		}
	}
	rows := s.History("pi", "cpu", ck.t.Add(-time.Hour), 600)
	if len(rows) < 5 || len(rows) > 7 {
		t.Fatalf("lignes : %d", len(rows))
	}
	n := 0
	for _, r := range rows {
		n += r.N
		if r.Min > r.Avg || r.Avg > r.Max {
			t.Fatalf("min/avg/max incohérents : %+v", r)
		}
	}
	if n < 55 {
		t.Fatalf("relevés perdus : %d", n)
	}
	if !s.HasSeries("pi", "cpu") || s.HasSeries("pi", "gpu") || s.SeriesCount() != 1 {
		t.Fatal("comptage des séries")
	}
}

func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	s, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		_ = s.AddAt("pi", "cpu", 40+float64(i), ck.t.Add(time.Duration(i-30)*time.Minute))
	}
	s.Maintain()
	s.Close()

	s2, err := OpenStore(dir, StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows := s2.History("pi", "cpu", ck.t.Add(-time.Hour), 0)
	if len(rows) == 0 {
		t.Fatal("rien relu après redémarrage")
	}
	total := 0
	for _, r := range rows {
		total += r.N
	}
	if total != 30 {
		t.Fatalf("relevés relus : %d, attendu 30", total)
	}
}

func TestStoreRollupLongHistory(t *testing.T) {
	ck := newClock()
	s, err := OpenStore(t.TempDir(), StoreOptions{Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := ck.t.Add(-10 * 24 * time.Hour)
	end := ck.t
	for at := start; at.Before(end); at = at.Add(5 * time.Minute) {
		ck.t = at
		_ = s.AddAt("pi", "cpu", 55, at)
		if at.Minute() == 0 {
			s.Maintain()
		}
	}
	ck.t = end
	s.Maintain()
	rows := s.History("pi", "cpu", start, 3600)
	if len(rows) < 200 {
		t.Fatalf("historique de 10 j trop court : %d lignes", len(rows))
	}
	for _, r := range rows {
		if r.Avg != 55 || r.N < 1 {
			t.Fatalf("ligne incorrecte : %+v", r)
		}
	}
}

func TestRegroupWeighted(t *testing.T) {
	rows := []Row{
		{T: 0, Avg: 10, Min: 8, Max: 12, N: 1},
		{T: 60, Avg: 20, Min: 18, Max: 30, N: 3},
		{T: 600, Avg: 5, Min: 5, Max: 5, N: 1},
	}
	out := regroup(rows, 600)
	if len(out) != 2 || out[0].N != 4 || out[0].Avg != 17.5 || out[0].Min != 8 || out[0].Max != 30 {
		t.Fatalf("regroupement : %+v", out)
	}
}

func TestNiceStep(t *testing.T) {
	if got := niceStep(3600, 60); got != 60 {
		t.Errorf("1h : %d", got)
	}
	if got := niceStep(86400, 240); got != 600 {
		t.Errorf("24h : %d", got)
	}
	if got := niceStep(365*86400, 240); got != 86400 {
		t.Errorf("1 an : %d", got)
	}
}
