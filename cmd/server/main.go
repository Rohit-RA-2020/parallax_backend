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
	"parallax/internal/elevenlabs"
	"parallax/internal/embed"
	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
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

	bins := ffmpeg.Bins{
		FFmpeg:  cfg.FFmpegBin,
		FFprobe: cfg.FFprobeBin,
	}
	detectCtx, detectCancel := context.WithTimeout(context.Background(), 45*time.Second)
	bins.Accel = ffmpeg.DetectAccel(detectCtx, bins, ffmpeg.DetectOpts{
		Prefer: cfg.FFmpegHWAccel,
		Device: cfg.FFmpegHWDevice,
	})
	detectCancel()
	if bins.Accel.Enabled() {
		log.Info("ffmpeg gpu encode enabled",
			"backend", bins.Accel.Backend,
			"device", bins.Accel.Device,
			"label", bins.Accel.Label,
			"h264", bins.Accel.H264,
			"hevc", bins.Accel.HEVC,
			"vp9", bins.Accel.VP9,
			"av1", bins.Accel.AV1,
		)
	} else {
		log.Info("ffmpeg gpu encode disabled", "prefer", cfg.FFmpegHWAccel)
	}

	systemPrompt := agent.SystemPromptAt(time.Now())
	if note := bins.Accel.PromptNote(); note != "" {
		systemPrompt += "\n" + note
	}

	reg := tools.NewRegistry()
	tools.RegisterMedia(reg, tools.MediaEnv{
		Workspace: cfg.WorkspaceDir,
		Bins:      bins,
	})
	tools.RegisterWeb(reg, tools.WebEnv{APIKey: cfg.ExaAPIKey, BaseURL: cfg.ExaBaseURL})
	tools.RegisterImage(reg, tools.ImageEnv{
		Workspace: cfg.WorkspaceDir,
		APIKey:    cfg.GeminiAPIKey,
		BaseURL:   cfg.GeminiBaseURL,
		Model:     cfg.GeminiImageModel,
	})
	projectStore, err := projects.NewStore(cfg.WorkspaceDir + "/projects")
	if err != nil {
		log.Error("projects", "err", err)
		os.Exit(1)
	}
	settings := config.NewStore(cfg.SettingsPath, cfg.LLMs)
	var geminiMusic *gemini.Client
	if cfg.GeminiAPIKey != "" {
		geminiMusic = gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiBaseURL, 15*time.Minute, 256<<20)
	}
	var elevenClient *elevenlabs.Client
	var elevenVoices *elevenlabs.VoiceCatalog
	if cfg.ElevenLabsAPIKey != "" {
		elevenClient = elevenlabs.NewClient(cfg.ElevenLabsAPIKey, cfg.ElevenLabsBaseURL, cfg.ElevenLabsRequestTimeout, cfg.ElevenLabsMaxResponseBytes)
	}
	var voiceErr error
	elevenVoices, voiceErr = elevenlabs.LoadVoiceCatalog(cfg.ElevenLabsVoicesFile)
	if voiceErr != nil {
		log.Error("ElevenLabs voice catalog", "err", voiceErr)
		elevenVoices = &elevenlabs.VoiceCatalog{}
	}
	idx := &transcript.Indexer{
		Projects: projectStore,
		Bins:     bins,
		Qdrant:   qdrant.NewClient(cfg.QdrantURL, cfg.QdrantAPIKey),
		Completer: func() llm.Completer {
			return llm.NewCompatClient(settings.Get().BaseURL, settings.Get().APIKey, settings.Get().Model)
		},
		Logger: log,
	}
	if whisperConfigured(cfg) {
		idx.Whisper = &transcript.FasterWhisper{
			Python:  cfg.WhisperPython,
			Script:  cfg.WhisperScript,
			Model:   cfg.WhisperModel,
			Device:  cfg.WhisperDevice,
			Compute: cfg.WhisperCompute,
		}
	} else {
		log.Info("transcript indexing disabled", "reason", "faster-whisper script is missing")
	}
	if err := config.ValidateEmbedding(cfg.Embedding); err != nil {
		log.Info("embeddings disabled", "reason", err.Error())
	} else {
		idx.Embeddings = embed.NewClient(cfg.Embedding.BaseURL, cfg.Embedding.APIKey, cfg.Embedding.Model)
	}
	idx.Start()
	indexer := idx

	srv := &httpapi.Server{
		Addr:                    cfg.Addr,
		Settings:                settings,
		Sessions:                agent.NewStore(),
		Tools:                   reg,
		SystemPrompt:            systemPrompt,
		ExaAPIKey:               cfg.ExaAPIKey,
		ExaBaseURL:              cfg.ExaBaseURL,
		GeminiAPIKey:            cfg.GeminiAPIKey,
		GeminiBaseURL:           cfg.GeminiBaseURL,
		GeminiImageModel:        cfg.GeminiImageModel,
		GeminiMusic:             geminiMusic,
		GeminiMusicModel:        cfg.GeminiMusicModel,
		GeminiMusicOutputFormat: cfg.GeminiMusicOutputFormat,
		Bins:                    bins,
		Projects:                projectStore,
		MaxIters:                cfg.MaxIters,
		Logger:                  log,
		Workspace:               cfg.WorkspaceDir,
		Indexer:                 indexer,
		ElevenLabs:              elevenClient,
		ElevenVoices:            elevenVoices,
		ElevenTTSModel:          cfg.ElevenLabsTTSModel,
		ElevenSFXModel:          cfg.ElevenLabsSFXModel,
		ElevenTTSOutputFormat:   cfg.ElevenLabsTTSOutputFormat,
		ElevenSFXOutputFormat:   cfg.ElevenLabsSFXOutputFormat,
		ElevenLimiter:           tools.NewLimiter(cfg.ElevenLabsMaxConcurrency),
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
