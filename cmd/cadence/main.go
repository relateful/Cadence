package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/greed/cadence/internal/analysis"
	"github.com/greed/cadence/internal/api"
	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/audiocache"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/config"
	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/session"
	"github.com/greed/cadence/internal/source"
	"github.com/greed/cadence/internal/source/applemusic"
	"github.com/greed/cadence/internal/source/deezer"
	"github.com/greed/cadence/internal/source/spotify"
	"github.com/greed/cadence/internal/voice"
)

const shutdownGrace = 10 * time.Second

func level(raw string) slog.Level {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func main() {
	if err := run(); err != nil {
		slog.Error("cadence stopped", "error", err)
		os.Exit(1)
	}
}

func nativeReady() error {
	if !voice.DaveAvailable() {
		return voice.ErrNoDave
	}
	enc, err := codec.NewEncoder(codec.DefaultBitrate)
	if err != nil {
		return err
	}
	enc.Close()
	return nil
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level(cfg.LogLevel)}))
	slog.SetDefault(log)
	if err := nativeReady(); err != nil {
		if os.Getenv("CADENCE_ALLOW_NO_VOICE") != "true" {
			return err
		}
		log.Error("voice is disabled in this build; only search and analysis will work", "error", err)
	}
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}

	clients := map[string]*httpx.Client{}
	for name, proxy := range map[string]string{deezer.Name: cfg.DeezerProxy, spotify.Name: cfg.SpotifyProxy, applemusic.Name: cfg.AppleProxy} {
		c, err := httpx.New(name, proxy, log)
		if err != nil {
			return err
		}
		clients[name] = c
	}
	if cfg.DeezerMasterKey != "" {
		dec, err := deezer.NewStripeDecryptor(cfg.DeezerMasterKey)
		if err != nil {
			return err
		}
		deezer.RegisterDecryptor(dec)
	}
	dz := deezer.New(clients[deezer.Name], cfg.DeezerARL, cfg.DeezerHighQuality, log)
	if cfg.AudioCacheDir != "" {
		store, err := audiocache.Open(cfg.AudioCacheDir, int64(cfg.AudioCacheGB)<<30, log)
		if err != nil {
			return err
		}
		dz.UseAudioCache(store)
	}
	resolver := source.NewResolver(dz, cfg.SearchOrder, log,
		applemusic.New(clients[applemusic.Name], cfg.AppleToken, cfg.AppleStorefront),
		spotify.New(clients[spotify.Name], cfg.SpotifyID, cfg.SpotifySecret),
	)

	registry := analysis.NewRegistry()
	offline := analysis.NewOffline(registry, func(ctx context.Context, t source.Track) (source.Stream, error) {
		playable, err := resolver.Playable(ctx, t)
		if err != nil {
			return source.Stream{}, err
		}
		return dz.Open(ctx, playable)
	}, cfg.AnalysisWorkers, cfg.AnalysisQueue, log)

	sessions := session.NewManager(session.Deps{
		Resolver:       resolver,
		Registry:       registry,
		BufferFrames:   cfg.BufferMs / audio.FrameMs,
		PlayoutDelay:   cfg.PlayoutDelay,
		UpdateInterval: cfg.UpdateInterval,
		Log:            log,
	})
	server := api.New(api.Deps{Password: cfg.Password, Sessions: sessions, Resolver: resolver, Registry: registry, Offline: offline, ResumeTimeout: cfg.ResumeTimeout, Bitrate: cfg.Bitrate, Log: log})

	httpServer := &http.Server{Addr: cfg.Address, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() {
		log.Info("cadence listening", "address", cfg.Address, "version", api.Version, "sources", resolver.Sources(), "dave", voice.DaveAvailable())
		errs <- httpServer.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	sessions.Shutdown()
	return httpServer.Shutdown(ctx)
}
