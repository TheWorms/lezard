package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // fuseaux horaires dans l'image scratch (plages calmes)
)

// version est remplacée à la compilation (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "interroge /api/health en local puis quitte (pour HEALTHCHECK Docker)")
	demo := flag.Bool("demo", false, "démarre avec des données fictives dans un dossier temporaire (aucun jeton requis)")
	showVersion := flag.Bool("version", false, "affiche la version puis quitte")
	flag.Parse()

	if *showVersion {
		fmt.Println("lezard", version)
		return
	}

	cfg, err := loadConfig(getenvOS)
	if err != nil {
		log.Fatalf("configuration : %v", err)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.Listen))
	}

	if *demo {
		cfg.DemoMode = true
		cfg.Token = "demo-token-demo-token"
		if cfg.Listen == ":3070" && os.Getenv("LEZARD_LISTEN") == "" {
			cfg.Listen = "127.0.0.1:3070"
		}
		cfg.Names = demoNames()
		cfg.PerHost = demoThresholds()
		if cfg.AdminPassword == "" {
			cfg.AdminPassword = "demo-admin"
			log.Printf("mode démo : mot de passe des réglages = demo-admin")
		}
	} else if len(cfg.Token) < 16 {
		log.Fatal("configuration : LEZARD_TOKEN manquant ou trop court (16 caractères minimum)")
	}

	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func run(cfg *Config) error {
	dataDir := cfg.DataDir
	if cfg.DemoMode {
		dir, err := os.MkdirTemp("", "lezard-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		dataDir = dir
	}

	store, err := OpenStore(dataDir, StoreOptions{RawRetention: time.Duration(cfg.RawDays) * 24 * time.Hour})
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.DemoMode {
		log.Printf("mode démo : génération de 7 jours de données fictives…")
		fillDemo(store)
		go liveDemo(ctx, store)
	}

	settings, err := OpenSettings(dataDir, cfg)
	if err != nil {
		return err
	}
	if cfg.AdminPassword == "" {
		log.Printf("LEZARD_ADMIN_PASSWORD non défini : la page Réglages est désactivée")
	}
	if cfg.DemoMode {
		demoTelegram()
	}
	alerter := NewAlerter(cfg, settings, store, telegramSend)
	alerter.SetContext(ctx)

	srv := NewServer(cfg, store, settings)
	srv.alerter = alerter
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	var bg sync.WaitGroup
	every(ctx, &bg, time.Minute, "maintenance du stockage", store.Maintain)
	every(ctx, &bg, 20*time.Second, "alertes", alerter.Check)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Printf("Lézard %s : écoute sur %s, données dans %s", version, cfg.Listen, dataDir)

	var runErr error
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
		log.Printf("arrêt demandé")
	}
	stop() // arrête aussi les tâches de fond si le serveur HTTP a échoué
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	bg.Wait() // les tâches de fond finissent leur passage avant la fermeture du stockage
	store.Maintain()
	return runErr
}

// every lance fn toutes les d jusqu'à l'annulation de ctx. Une panique dans fn est
// journalisée et n'arrête ni la tâche ni le programme.
func every(ctx context.Context, wg *sync.WaitGroup, d time.Duration, name string, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				safeRun(name, fn)
			}
		}
	}()
}

func safeRun(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("erreur interne (%s) : %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
}

func runHealthcheck(listen string) int {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		host, port = "", "3070"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1" // écoute sur toutes les interfaces : la boucle locale suffit
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/api/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
