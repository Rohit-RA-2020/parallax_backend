package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"parallax/internal/agent"
	"parallax/internal/config"
	"parallax/internal/ffmpeg"
	"parallax/internal/httpapi"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/tools"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	reg := tools.NewRegistry()
	tools.RegisterMedia(reg, tools.MediaEnv{
		Workspace: cfg.WorkspaceDir,
		Bins: ffmpeg.Bins{
			FFmpeg:  cfg.FFmpegBin,
			FFprobe: cfg.FFprobeBin,
		},
	})
	projectStore, err := projects.NewStore(cfg.WorkspaceDir + "/projects")
	if err != nil {
		log.Error("projects", "err", err)
		os.Exit(1)
	}

	srv := &httpapi.Server{
		Addr:     cfg.Addr,
		Settings: config.NewStore(cfg.SettingsPath, cfg.LLM),
		Sessions: agent.NewStore(),
		Tools:    reg,
		Bins: ffmpeg.Bins{
			FFmpeg:  cfg.FFmpegBin,
			FFprobe: cfg.FFprobeBin,
		},
		Projects:  projectStore,
		MaxIters:  cfg.MaxIters,
		Logger:    log,
		Workspace: cfg.WorkspaceDir,
		NewLLM: func(l config.LLM) llm.ChatProvider {
			return llm.NewCompatClient(l.BaseURL, l.APIKey, l.Model)
		},
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("parallax listening",
			"addr", cfg.Addr,
			"workspace", cfg.WorkspaceDir,
			"model", srv.Settings.Get().Model,
			"base_url", srv.Settings.Get().BaseURL,
		)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
}
