package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stockage en fichiers, sans dépendance.
//
//   - relevés bruts : un fichier par jour UTC (raw-AAAAMMJJ.tsv), gardés en
//     mémoire sur la durée de rétention brute (3 jours par défaut) ;
//   - agrégats de 10 minutes (m10.tsv) puis d'1 heure (h1.tsv) pour l'historique
//     long. Un agrégat n'est écrit que lorsque son créneau est terminé.
//
// Les lignes sont des TSV : un fichier tronqué par une coupure de courant perd au
// pire sa dernière ligne, les lignes illisibles sont ignorées au chargement.

const (
	m10Secs int64 = 600
	h1Secs  int64 = 3600
)

// Row est un relevé brut (N=1, Avg=Min=Max) ou un agrégat.
type Row struct {
	T   int64
	Avg float64
	Min float64
	Max float64
	N   int
}

type point struct {
	T int64
	C float64
}

type seriesKey struct {
	Host   string
	Sensor string
}

type series struct {
	raw     []point
	m10     []Row
	h1      []Row
	m10Next int64
	h1Next  int64
	lastT   int64
	lastC   float64
	hasLast bool
}

// StoreOptions règle les durées de conservation.
type StoreOptions struct {
	RawRetention time.Duration
	M10Retention time.Duration
	H1Retention  time.Duration
	Now          func() time.Time
}

func (o *StoreOptions) setDefaults() {
	if o.RawRetention < 24*time.Hour {
		o.RawRetention = 72 * time.Hour
	}
	if o.M10Retention <= o.RawRetention {
		o.M10Retention = 30 * 24 * time.Hour
	}
	if o.H1Retention <= o.M10Retention {
		o.H1Retention = 400 * 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Store garde l'historique des températures.
type Store struct {
	mu          sync.Mutex
	dir         string
	opt         StoreOptions
	series      map[seriesKey]*series
	rawF        *os.File
	rawDay      string
	rawBroken   bool // une écriture a échoué : le fichier peut finir par une ligne partielle
	m10F        *os.File
	h1F         *os.File
	dirty       bool
	lastCompact time.Time
	writeErrs   int
	lastErrAt   time.Time
}

// errTooManySeries : la limite de séries (LEZARD_MAX_SERIES) est atteinte.
var errTooManySeries = errors.New("nombre maximal de séries atteint")

// OpenStore charge les fichiers de dir (créé si besoin) et rattrape les agrégats.
func OpenStore(dir string, opt StoreOptions) (*Store, error) {
	opt.setDefaults()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("création du dossier de données : %w", err)
	}
	s := &Store{dir: dir, opt: opt, series: map[seriesKey]*series{}}
	if err := s.loadRollups(); err != nil {
		return nil, err
	}
	if err := s.loadRaw(); err != nil {
		return nil, err
	}
	var err error
	if s.m10F, err = openAppend(filepath.Join(dir, "m10.tsv")); err != nil {
		return nil, err
	}
	if s.h1F, err = openAppend(filepath.Join(dir, "h1.tsv")); err != nil {
		_ = s.m10F.Close()
		return nil, err
	}
	s.mu.Lock()
	s.maintainLocked(opt.Now())
	s.mu.Unlock()
	return s, nil
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("ouverture de %s : %w", path, err)
	}
	return f, nil
}

func (s *Store) get(k seriesKey) *series {
	sr := s.series[k]
	if sr == nil {
		sr = &series{}
		s.series[k] = sr
	}
	return sr
}

func scanLines(path string, fn func(fields []string)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	// Une ligne démesurée (bloc de zéros après une coupure de courant, fichier
	// corrompu) est ignorée au lieu d'empêcher le démarrage.
	const maxLine = 4096
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadSlice('\n')
		switch {
		case err == nil:
		case errors.Is(err, bufio.ErrBufferFull):
			for errors.Is(err, bufio.ErrBufferFull) { // jette le reste de la ligne
				_, err = br.ReadSlice('\n')
			}
			if err != nil {
				return nilIfEOF(err)
			}
			continue
		default: // fin de fichier (dernière ligne sans saut de ligne comprise) ou erreur
			if len(line) > 0 && len(line) <= maxLine {
				fn(strings.Split(strings.TrimRight(string(line), "\r\n"), "\t"))
			}
			return nilIfEOF(err)
		}
		if len(line) <= 1 || len(line) > maxLine {
			continue
		}
		fn(strings.Split(strings.TrimRight(string(line), "\r\n"), "\t"))
	}
}

func nilIfEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func parseRollupLine(f []string) (seriesKey, Row, bool) {
	if len(f) != 7 {
		return seriesKey{}, Row{}, false
	}
	t, e1 := strconv.ParseInt(f[0], 10, 64)
	avg, e2 := strconv.ParseFloat(f[3], 64)
	mn, e3 := strconv.ParseFloat(f[4], 64)
	mx, e4 := strconv.ParseFloat(f[5], 64)
	n, e5 := strconv.Atoi(f[6])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || n <= 0 || f[1] == "" || f[2] == "" {
		return seriesKey{}, Row{}, false
	}
	return seriesKey{f[1], f[2]}, Row{T: t, Avg: avg, Min: mn, Max: mx, N: n}, true
}

func (s *Store) loadRollups() error {
	load := func(name string, size int64, pick func(*series) *[]Row, next func(*series) *int64) error {
		err := scanLines(filepath.Join(s.dir, name), func(f []string) {
			if k, r, ok := parseRollupLine(f); ok {
				rows := pick(s.get(k))
				*rows = append(*rows, r)
			}
		})
		if err != nil {
			return fmt.Errorf("lecture de %s : %w", name, err)
		}
		for _, sr := range s.series {
			rows := pick(sr)
			if len(*rows) == 0 {
				continue
			}
			sort.SliceStable(*rows, func(i, j int) bool { return (*rows)[i].T < (*rows)[j].T })
			*rows = dedupeRows(*rows)
			*next(sr) = (*rows)[len(*rows)-1].T + size
		}
		return nil
	}
	if err := load("m10.tsv", m10Secs, func(sr *series) *[]Row { return &sr.m10 }, func(sr *series) *int64 { return &sr.m10Next }); err != nil {
		return err
	}
	return load("h1.tsv", h1Secs, func(sr *series) *[]Row { return &sr.h1 }, func(sr *series) *int64 { return &sr.h1Next })
}

// dedupeRows garde la dernière ligne pour un même créneau.
func dedupeRows(rows []Row) []Row {
	out := rows[:0]
	for _, r := range rows {
		if n := len(out); n > 0 && out[n-1].T == r.T {
			out[n-1] = r
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *Store) loadRaw() error {
	files, _ := filepath.Glob(filepath.Join(s.dir, "raw-*.tsv"))
	sort.Strings(files)
	cutoff := s.opt.Now().Add(-s.opt.RawRetention).Unix()
	for _, path := range files {
		err := scanLines(path, func(f []string) {
			if len(f) != 4 {
				return
			}
			t, e1 := strconv.ParseInt(f[0], 10, 64)
			c, e2 := strconv.ParseFloat(f[3], 64)
			if e1 != nil || e2 != nil || t < cutoff || f[1] == "" || f[2] == "" || math.IsNaN(c) || math.IsInf(c, 0) {
				return
			}
			sr := s.get(seriesKey{f[1], f[2]})
			sr.raw = append(sr.raw, point{T: t, C: c})
		})
		if err != nil {
			return fmt.Errorf("lecture de %s : %w", filepath.Base(path), err)
		}
	}
	for _, sr := range s.series {
		sort.SliceStable(sr.raw, func(i, j int) bool { return sr.raw[i].T < sr.raw[j].T })
		sr.raw = dedupePoints(sr.raw)
		sr.refreshLast()
	}
	return nil
}

func dedupePoints(p []point) []point {
	out := p[:0]
	for _, x := range p {
		if n := len(out); n > 0 && out[n-1].T == x.T {
			out[n-1] = x
			continue
		}
		out = append(out, x)
	}
	return out
}

// refreshLast recalcule le dernier relevé connu (brut, sinon dernier agrégat).
func (sr *series) refreshLast() {
	switch {
	case len(sr.raw) > 0:
		p := sr.raw[len(sr.raw)-1]
		sr.lastT, sr.lastC, sr.hasLast = p.T, p.C, true
	case len(sr.m10) > 0:
		r := sr.m10[len(sr.m10)-1]
		sr.lastT, sr.lastC, sr.hasLast = r.T, r.Avg, true
	case len(sr.h1) > 0:
		r := sr.h1[len(sr.h1)-1]
		sr.lastT, sr.lastC, sr.hasLast = r.T, r.Avg, true
	}
}

// Add enregistre un relevé daté de maintenant.
func (s *Store) Add(host, sensor string, c float64) error {
	return s.AddAt(host, sensor, c, s.opt.Now())
}

// AddLimited enregistre un relevé daté de maintenant, sauf s'il crée une nouvelle
// série alors que maxSeries sont déjà connues (vérification et ajout atomiques).
func (s *Store) AddLimited(host, sensor string, c float64, maxSeries int) error {
	return s.addAt(host, sensor, c, s.opt.Now(), maxSeries)
}

// AddAt enregistre un relevé daté de at.
func (s *Store) AddAt(host, sensor string, c float64, at time.Time) error {
	return s.addAt(host, sensor, c, at, 0)
}

func (s *Store) addAt(host, sensor string, c float64, at time.Time, maxSeries int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := at.Unix()
	k := seriesKey{host, sensor}
	if _, ok := s.series[k]; !ok && maxSeries > 0 && len(s.series) >= maxSeries {
		return errTooManySeries
	}
	sr := s.get(k)
	i := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].T >= t })
	if i < len(sr.raw) && sr.raw[i].T == t && sr.raw[i].C == c {
		return nil // même relevé reçu deux fois dans la seconde : rien à écrire (protège le disque)
	}
	switch {
	case i < len(sr.raw) && sr.raw[i].T == t:
		sr.raw[i].C = c
	case i == len(sr.raw):
		sr.raw = append(sr.raw, point{T: t, C: c})
	default:
		sr.raw = append(sr.raw, point{})
		copy(sr.raw[i+1:], sr.raw[i:])
		sr.raw[i] = point{T: t, C: c}
	}
	if !sr.hasLast || t >= sr.lastT {
		sr.lastT, sr.lastC, sr.hasLast = t, c, true
	}
	if err := s.writeRawLocked(t, host, sensor, c); err != nil {
		s.noteWriteErrLocked()
		return err
	}
	return nil
}

// NoteWriteError signale un échec d'écriture disque survenu hors du stockage
// (réglages, état des alertes) pour que /api/health passe en « degraded ».
func (s *Store) NoteWriteError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteWriteErrLocked()
}

func (s *Store) noteWriteErrLocked() {
	s.writeErrs++
	s.lastErrAt = s.opt.Now()
}

func (s *Store) writeRawLocked(t int64, host, sensor string, c float64) error {
	day := time.Unix(t, 0).UTC().Format("20060102")
	if s.rawBroken && s.rawF != nil {
		_ = s.rawF.Close()
		s.rawF = nil
	}
	if s.rawF == nil || s.rawDay != day {
		if s.rawF != nil {
			_ = s.rawF.Close()
			s.rawF = nil
		}
		f, err := openAppend(filepath.Join(s.dir, "raw-"+day+".tsv"))
		if err != nil {
			return err
		}
		s.rawF, s.rawDay = f, day
		if s.rawBroken {
			// termine d'abord une éventuelle ligne partielle laissée par l'échec précédent
			if _, err := s.rawF.WriteString("\n"); err != nil {
				_ = s.rawF.Close()
				s.rawF = nil
				return err
			}
			s.rawBroken = false
		}
	}
	_, err := s.rawF.WriteString(strconv.FormatInt(t, 10) + "\t" + host + "\t" + sensor + "\t" + strconv.FormatFloat(c, 'f', 2, 64) + "\n")
	if err != nil {
		s.rawBroken = true
	}
	return err
}

// Maintain finalise les agrégats terminés, purge ce qui a expiré et compacte les
// fichiers d'agrégats. À appeler régulièrement (chaque minute).
func (s *Store) Maintain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintainLocked(s.opt.Now())
}

func floorTo(t, size int64) int64 {
	if t >= 0 {
		return t / size * size
	}
	return -((-t + size - 1) / size * size)
}

func aggregate(p []point) Row {
	r := Row{Min: p[0].C, Max: p[0].C, N: len(p)}
	sum := 0.0
	for _, x := range p {
		sum += x.C
		if x.C < r.Min {
			r.Min = x.C
		}
		if x.C > r.Max {
			r.Max = x.C
		}
	}
	r.Avg = sum / float64(len(p))
	return r
}

func (s *Store) rollTier(k seriesKey, sr *series, size int64, next *int64, rows *[]Row, f *os.File, nowT int64) {
	if *next == 0 {
		if len(sr.raw) == 0 {
			return
		}
		*next = floorTo(sr.raw[0].T, size)
	}
	for *next+size <= nowT {
		lo := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].T >= *next })
		if lo == len(sr.raw) {
			*next = floorTo(nowT, size)
			return
		}
		if sr.raw[lo].T >= *next+size {
			*next = floorTo(sr.raw[lo].T, size)
			continue
		}
		hi := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].T >= *next+size })
		row := aggregate(sr.raw[lo:hi])
		row.T = *next
		*rows = append(*rows, row)
		if f != nil {
			line := fmt.Sprintf("%d\t%s\t%s\t%.3f\t%.2f\t%.2f\t%d\n", row.T, k.Host, k.Sensor, row.Avg, row.Min, row.Max, row.N)
			if _, err := f.WriteString(line); err != nil {
				s.noteWriteErrLocked()
			}
		}
		*next += size
	}
}

func (s *Store) maintainLocked(now time.Time) {
	nowT := now.Unix()
	// fichiers d'agrégats perdus après une erreur disque : on retente de les ouvrir
	for _, x := range []struct {
		f    **os.File
		name string
	}{{&s.m10F, "m10.tsv"}, {&s.h1F, "h1.tsv"}} {
		if *x.f == nil {
			if f, err := openAppend(filepath.Join(s.dir, x.name)); err == nil {
				*x.f = f
			}
		}
	}
	rawCut := nowT - int64(s.opt.RawRetention/time.Second)
	m10Cut := nowT - int64(s.opt.M10Retention/time.Second)
	h1Cut := nowT - int64(s.opt.H1Retention/time.Second)
	for k, sr := range s.series {
		s.rollTier(k, sr, m10Secs, &sr.m10Next, &sr.m10, s.m10F, nowT)
		s.rollTier(k, sr, h1Secs, &sr.h1Next, &sr.h1, s.h1F, nowT)

		if i := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].T >= rawCut }); i > 0 {
			sr.raw = append([]point(nil), sr.raw[i:]...)
		}
		if i := sort.Search(len(sr.m10), func(i int) bool { return sr.m10[i].T >= m10Cut }); i > 0 {
			sr.m10 = append([]Row(nil), sr.m10[i:]...)
			s.dirty = true
		}
		if i := sort.Search(len(sr.h1), func(i int) bool { return sr.h1[i].T >= h1Cut }); i > 0 {
			sr.h1 = append([]Row(nil), sr.h1[i:]...)
			s.dirty = true
		}
		if !sr.hasLast {
			sr.refreshLast()
		}
		if len(sr.raw) == 0 && len(sr.m10) == 0 && len(sr.h1) == 0 {
			delete(s.series, k) // plus aucune donnée : la série ne compte plus dans la limite
		}
	}
	if s.dirty && now.Sub(s.lastCompact) > 6*time.Hour {
		s.compactLocked()
		s.lastCompact = now
	}
	s.removeOldRawFilesLocked(now)
	for _, f := range []*os.File{s.rawF, s.m10F, s.h1F} {
		if f != nil {
			if err := f.Sync(); err != nil {
				s.noteWriteErrLocked() // disque plein ou en erreur : les relevés ne sont pas durables
			}
		}
	}
}

func (s *Store) removeOldRawFilesLocked(now time.Time) {
	limit := now.Add(-s.opt.RawRetention - 24*time.Hour).UTC().Format("20060102")
	files, _ := filepath.Glob(filepath.Join(s.dir, "raw-*.tsv"))
	for _, path := range files {
		day := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "raw-"), ".tsv")
		if len(day) == 8 && day < limit {
			_ = os.Remove(path)
		}
	}
}

// compactLocked réécrit m10.tsv et h1.tsv depuis la mémoire (atomique).
func (s *Store) compactLocked() {
	keys := make([]seriesKey, 0, len(s.series))
	for k := range s.series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Host != keys[j].Host {
			return keys[i].Host < keys[j].Host
		}
		return keys[i].Sensor < keys[j].Sensor
	})
	rewrite := func(name string, cur **os.File, pick func(*series) []Row) {
		path := filepath.Join(s.dir, name)
		tmp := path + ".tmp"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			s.noteWriteErrLocked()
			return
		}
		w := bufio.NewWriter(out)
		for _, k := range keys {
			for _, r := range pick(s.series[k]) {
				fmt.Fprintf(w, "%d\t%s\t%s\t%.3f\t%.2f\t%.2f\t%d\n", r.T, k.Host, k.Sensor, r.Avg, r.Min, r.Max, r.N)
			}
		}
		err = w.Flush()
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			s.noteWriteErrLocked()
			_ = os.Remove(tmp)
			return
		}
		if *cur != nil {
			_ = (*cur).Close()
		}
		if err := os.Rename(tmp, path); err != nil {
			s.noteWriteErrLocked()
			_ = os.Remove(tmp)
		} else {
			syncDir(s.dir)
		}
		if f, err := openAppend(path); err == nil {
			*cur = f
		} else {
			*cur = nil
			s.noteWriteErrLocked()
		}
	}
	rewrite("m10.tsv", &s.m10F, func(sr *series) []Row { return sr.m10 })
	rewrite("h1.tsv", &s.h1F, func(sr *series) []Row { return sr.h1 })
	s.dirty = false
}

// Close vide les tampons et ferme les fichiers.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, f := range []*os.File{s.rawF, s.m10F, s.h1F} {
		if f == nil {
			continue
		}
		if err := f.Sync(); err != nil && first == nil {
			first = err
		}
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.rawF, s.m10F, s.h1F = nil, nil, nil
	return first
}

// WriteErrors renvoie le nombre d'échecs d'écriture disque depuis le démarrage
// et l'heure du dernier (zéro s'il n'y en a pas eu).
func (s *Store) WriteErrors() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErrs, s.lastErrAt
}

// SeriesCount renvoie le nombre de séries connues.
func (s *Store) SeriesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.series)
}

// HasSeries indique si la série existe déjà.
func (s *Store) HasSeries(host, sensor string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.series[seriesKey{host, sensor}]
	return ok
}

// SeriesView est la vue d'une série pour la page d'accueil.
type SeriesView struct {
	Host   string
	Sensor string
	LastT  int64
	LastC  float64
	HasRng bool
	Min    float64
	Max    float64
	Avg    float64
	Spark  []Row
}

// Overview renvoie une vue de chaque série relevée depuis minLast (epoch) : le
// dernier relevé, et l'historique des rng dernières secondes regroupé par pas de
// step secondes, dont sont tirés min, max et moyenne.
func (s *Store) Overview(now time.Time, minLast int64, rng int64, step int64) []SeriesView {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowT := now.Unix()
	out := make([]SeriesView, 0, len(s.series))
	for k, sr := range s.series {
		if !sr.hasLast || sr.lastT < minLast {
			continue
		}
		v := SeriesView{Host: k.Host, Sensor: k.Sensor, LastT: sr.lastT, LastC: sr.lastC}
		v.Spark = s.historyLocked(sr, nowT-rng, nowT, step, nowT)
		if len(v.Spark) > 0 {
			v.HasRng = true
			v.Min, v.Max = v.Spark[0].Min, v.Spark[0].Max
			sum, n := 0.0, 0
			for _, r := range v.Spark {
				if r.Min < v.Min {
					v.Min = r.Min
				}
				if r.Max > v.Max {
					v.Max = r.Max
				}
				sum += r.Avg * float64(r.N)
				n += r.N
			}
			v.Avg = sum / float64(n)
		}
		out = append(out, v)
	}
	return out
}

// Sensors liste les capteurs connus d'une machine, triés.
func (s *Store) Sensors(host string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.series {
		if k.Host == host {
			out = append(out, k.Sensor)
		}
	}
	sort.Strings(out)
	return out
}

// History renvoie l'historique d'une série entre since et maintenant, regroupé
// par créneaux de step secondes (0 = résolution native).
func (s *Store) History(host, sensor string, since time.Time, step int64) []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := s.series[seriesKey{host, sensor}]
	if sr == nil {
		return nil
	}
	nowT := s.opt.Now().Unix()
	return s.historyLocked(sr, since.Unix(), nowT, step, nowT)
}

// historyLocked prend, pour chaque instant, le niveau le plus fin disponible :
// brut sur la rétention brute, agrégats de 10 min jusqu'à leur rétention, puis
// agrégats d'1 h.
func (s *Store) historyLocked(sr *series, since, until, step, nowT int64) []Row {
	rawFrom := nowT - int64(s.opt.RawRetention/time.Second)
	m10From := nowT - int64(s.opt.M10Retention/time.Second)
	var rows []Row
	for _, r := range sr.h1 {
		if r.T >= since && r.T < m10From && r.T <= until {
			rows = append(rows, r)
		}
	}
	lo := since
	if m10From > lo {
		lo = m10From
	}
	for _, r := range sr.m10 {
		if r.T >= lo && r.T < rawFrom && r.T <= until {
			rows = append(rows, r)
		}
	}
	lo = since
	if rawFrom > lo {
		lo = rawFrom
	}
	i := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].T >= lo })
	for ; i < len(sr.raw) && sr.raw[i].T <= until; i++ {
		p := sr.raw[i]
		rows = append(rows, Row{T: p.T, Avg: p.C, Min: p.C, Max: p.C, N: 1})
	}
	return regroup(rows, step)
}

// regroup fusionne des lignes triées par créneaux de step secondes (moyenne
// pondérée par N, min et max conservés).
func regroup(rows []Row, step int64) []Row {
	if step <= 0 || len(rows) == 0 {
		return rows
	}
	out := make([]Row, 0, len(rows))
	sums := make([]float64, 0, len(rows))
	for _, r := range rows {
		g := floorTo(r.T, step)
		if n := len(out); n > 0 && out[n-1].T == g {
			o := &out[n-1]
			sums[n-1] += r.Avg * float64(r.N)
			o.N += r.N
			if r.Min < o.Min {
				o.Min = r.Min
			}
			if r.Max > o.Max {
				o.Max = r.Max
			}
			continue
		}
		out = append(out, Row{T: g, Min: r.Min, Max: r.Max, N: r.N})
		sums = append(sums, r.Avg*float64(r.N))
	}
	for i := range out {
		out[i].Avg = sums[i] / float64(out[i].N)
	}
	return out
}

// niceStep choisit un pas lisible pour viser environ target points.
func niceStep(rangeSecs int64, target int64) int64 {
	if target <= 0 {
		target = 240
	}
	want := (rangeSecs + target - 1) / target
	for _, c := range []int64{60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400} {
		if c >= want {
			return c
		}
	}
	return 86400
}

// Latest est le dernier relevé d'une série.
type Latest struct {
	Host   string
	Sensor string
	T      int64
	C      float64
}

// LatestAll renvoie le dernier relevé de chaque série vue depuis minLast.
func (s *Store) LatestAll(minLast int64) []Latest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Latest, 0, len(s.series))
	for k, sr := range s.series {
		if sr.hasLast && sr.lastT >= minLast {
			out = append(out, Latest{Host: k.Host, Sensor: k.Sensor, T: sr.lastT, C: sr.lastC})
		}
	}
	return out
}

// Dir renvoie le dossier des données.
func (s *Store) Dir() string { return s.dir }
