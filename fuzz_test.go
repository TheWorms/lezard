package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// go test -fuzz=FuzzPush -fuzztime=30s
func FuzzPush(f *testing.F) {
	f.Add("pi", "cpu", "48.5")
	f.Add("NAS", "", "52,3")
	f.Add("a\tb", "c\nd", "NaN")
	f.Add("x", "y", "1e308")
	f.Fuzz(func(t *testing.T, host, sensor, temp string) {
		s, st, _ := newTestServer(t)
		code := push(s, "Bearer "+testToken, url.Values{"host": {host}, "sensor": {sensor}, "temp": {temp}})
		if code != 204 && code != 400 {
			t.Fatalf("code inattendu %d pour %q %q %q", code, host, sensor, temp)
		}
		// le fichier brut ne doit contenir que des lignes à 4 champs
		files, _ := filepath.Glob(filepath.Join(st.Dir(), "raw-*.tsv"))
		for _, p := range files {
			b, _ := os.ReadFile(p)
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if line != "" && len(strings.Split(line, "\t")) != 4 {
					t.Fatalf("ligne corrompue : %q", line)
				}
			}
		}
	})
}

// go test -fuzz=FuzzRollupLine -fuzztime=30s
func FuzzRollupLine(f *testing.F) {
	f.Add("1700000000\tpi\tcpu\t50.000\t45.00\t55.00\t10")
	f.Add("x\ty")
	f.Fuzz(func(t *testing.T, line string) {
		_, _, _ = parseRollupLine(strings.Split(line, "\t"))
	})
}
