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
	"parallax/internal/embed"
	"parallax/internal/ffmpeg"
	"parallax/internal/httpapi"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
	"parallax/internal/tools"
	"parallax/internal/transcript"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	systemPrompt := agent.SystemPromptAt(time.Now())

	reg := tools.NewRegistry()
	tools.RegisterMedia(reg, tools.MediaEnv{
		Workspace: cfg.WorkspaceDir,
		Bins: ffmpeg.Bins{
			FFmpeg:  cfg.FFmpegBin,
			FFprobe: cfg.FFprobeBin,
		},
	})
	tools.RegisterWeb(reg, tools.WebEnv{APIKey: cfg.ExaAPIKey, BaseURL: cfg.ExaBaseURL})
	projectStore, err := projects.NewStore(cfg.WorkspaceDir + "/projects")
	if err != nil {
		log.Error("projects", "err", err)
		os.Exit(1)
	}

	bins := ffmpeg.Bins{
		FFmpeg:  cfg.FFmpegBin,
		FFprobe: cfg.FFprobeBin,
	}
	settings := config.NewStore(cfg.SettingsPath, cfg.LLMs)
	var indexer *transcript.Indexer
	if whisperConfigured(cfg) {
		idx := &transcript.Indexer{
			Projects: projectStore,
			Bins:     bins,
			Whisper: &transcript.FasterWhisper{
				Python:  cfg.WhisperPython,
				Script:  cfg.WhisperScript,
				Model:   cfg.WhisperModel,
				Device:  cfg.WhisperDevice,
				Compute: cfg.WhisperCompute,
			},
			Qdrant: qdrant.NewClient(cfg.QdrantURL, cfg.QdrantAPIKey),
			Completer: func() llm.Completer {
				return llm.NewCompatClient(settings.Get().BaseURL, settings.Get().APIKey, settings.Get().Model)
			},
			Logger: log,
		}
		if err := config.ValidateEmbedding(cfg.Embedding); err != nil {
			log.Info("transcript embeddings disabled", "reason", err.Error())
		} else {
			idx.Embeddings = embed.NewClient(cfg.Embedding.BaseURL, cfg.Embedding.APIKey, cfg.Embedding.Model)
		}
		idx.Start()
		indexer = idx
	} else {
		log.Info("transcript indexing disabled", "reason", "faster-whisper script is missing")
	}

	srv := &httpapi.Server{
		Addr:         cfg.Addr,
		Settings:     settings,
		Sessions:     agent.NewStore(),
		Tools:        reg,
		SystemPrompt: systemPrompt,
		ExaAPIKey:    cfg.ExaAPIKey,
		ExaBaseURL:   cfg.ExaBaseURL,
		Bins:         bins,
		Projects:     projectStore,
		MaxIters:     cfg.MaxIters,
		Logger:       log,
		Workspace:    cfg.WorkspaceDir,
		Indexer:      indexer,
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
	if indexer != nil {
		indexer.Close()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
}

func whisperConfigured(cfg config.Config) bool {
	_, err := os.Stat(cfg.WhisperScript)
	return err == nil
}
