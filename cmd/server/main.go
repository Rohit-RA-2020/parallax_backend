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
	"parallax/internal/auth"
	"parallax/internal/config"
	"parallax/internal/database"
	"parallax/internal/elevenlabs"
	"parallax/internal/embed"
	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
	"parallax/internal/gifs"
	"parallax/internal/httpapi"
	"parallax/internal/llm"
	"parallax/internal/objectstore"
	"parallax/internal/preview"
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
	if err := cfg.ValidateInfrastructure(); err != nil {
		log.Error("infrastructure config", "err", err)
		os.Exit(1)
	}
	db, err := database.Open(context.Background(), cfg.DatabaseURL, log)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	objects, err := objectstore.New(cfg.MediaStorageDir)
	if err != nil {
		log.Error("durable media storage", "err", err)
		os.Exit(1)
	}
	authenticator, err := auth.New(context.Background(), auth.Config{JWKSURL: cfg.SupabaseJWKSURL, Issuer: cfg.SupabaseIssuer, Audience: cfg.SupabaseAudience, MediaSecret: cfg.MediaCookieSecret, MediaCookieSecure: cfg.MediaCookieSecure}, db.Pool)
	if err != nil {
		log.Error("supabase auth", "err", err)
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
	gifService := gifs.New(gifs.Config{
		GiphyAPIKey: cfg.GiphyAPIKey, GiphyBaseURL: cfg.GiphyBaseURL,
		KlipyAPIKey: cfg.KlipyAPIKey, KlipyBaseURL: cfg.KlipyBaseURL,
	})
	tools.RegisterImage(reg, tools.ImageEnv{
		Workspace: cfg.WorkspaceDir,
		APIKey:    cfg.GeminiAPIKey,
		BaseURL:   cfg.GeminiBaseURL,
		Model:     cfg.GeminiImageModel,
	})
	projectStore, err := projects.NewPostgresStore(cfg.WorkspaceDir+"/projects", db.Pool, objects)
	if err != nil {
		log.Error("projects", "err", err)
		os.Exit(1)
	}
	// Production model profiles are immutable environment configuration. User
	// selections live in user_preferences, never in the old global settings file.
	settings := config.NewStore("", cfg.LLMs)
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
		Database: db,
		Bins:     bins,
		Qdrant:   qdrant.NewSharedClient(cfg.QdrantURL, cfg.QdrantAPIKey, cfg.QdrantCollection),
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

	previews := &preview.Builder{Projects: projectStore, Bins: bins, Logger: log}
	previews.Start()
	uploads, err := httpapi.NewUploadManager(httpapi.UploadManagerConfig{
		Workspace: cfg.WorkspaceDir, Projects: projectStore, Bins: bins, Logger: log,
		MaxSize: cfg.MaxUploadBytes, MaxTempBytes: cfg.TempMaxBytes, Expiry: cfg.UploadExpiry,
		StatusRetention: cfg.UploadStatusRetention,
		MaxActive:       cfg.MaxActiveUploads, MaxPerProject: cfg.MaxProjectUploads,
		Database: db,
		OnReady: func(projectID string, media projects.Media, uploadMs int64) {
			indexer.NoteUpload(projectID, media.Path, uploadMs)
			indexer.Enqueue(projectID, media.Path)
			previews.Enqueue(projectID, media.Path)
		},
	})
	if err != nil {
		log.Error("uploads", "err", err)
		os.Exit(1)
	}

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
		GeminiOmniVideoModel:    cfg.GeminiOmniVideoModel,
		GeminiVeoVideoModel:     cfg.GeminiVeoVideoModel,
		GeminiVideoTimeout:      cfg.GeminiVideoTimeout,
		GeminiVideoPoll:         cfg.GeminiVideoPoll,
		GIFs:                    gifService,
		GeminiMusic:             geminiMusic,
		GeminiMusicModel:        cfg.GeminiMusicModel,
		GeminiMusicOutputFormat: cfg.GeminiMusicOutputFormat,
		Bins:                    bins,
		Projects:                projectStore,
		MaxIters:                cfg.MaxIters,
		MaxParallelTools:        cfg.MaxParallelTools,
		Logger:                  log,
		Workspace:               cfg.WorkspaceDir,
		ProviderIconsDir:        cfg.ProviderIconsDir,
		Indexer:                 indexer,
		Previews:                previews,
		ElevenLabs:              elevenClient,
		ElevenVoices:            elevenVoices,
		ElevenTTSModel:          cfg.ElevenLabsTTSModel,
		ElevenSFXModel:          cfg.ElevenLabsSFXModel,
		ElevenTTSOutputFormat:   cfg.ElevenLabsTTSOutputFormat,
		ElevenSFXOutputFormat:   cfg.ElevenLabsSFXOutputFormat,
		ElevenLimiter:           tools.NewLimiter(cfg.ElevenLabsMaxConcurrency),
		YouTubeTimeout:          cfg.YouTubeDownloadTimeout,
		YouTubeMaxBytes:         cfg.YouTubeMaxDownloadBytes,
		YouTubeYTDLPBin:         cfg.YouTubeYTDLPBin,
		BlenderBin:              cfg.BlenderBin,
		BlenderBridgeHost:       cfg.BlenderBridgeHost,
		BlenderBridgePort:       cfg.BlenderBridgePort,
		BlenderTimeout:          cfg.BlenderTimeout,
		Uploads:                 uploads,
		Auth:                    authenticator,
		Database:                db,
		Objects:                 objects,
		AllowedOrigins:          cfg.AllowedOrigins,
		NewLLM: func(l config.LLM) llm.ChatProvider {
			return llm.NewCompatClient(l.BaseURL, l.APIKey, l.Model)
		},
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		// ReadTimeout and WriteTimeout stay unset so multi-GB uploads and
		// long transcodes are not killed mid-stream.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go runPurgeWorker(ctx, db, objects, uploads, indexer, cfg.WorkspaceDir, log)

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
	if uploads != nil {
		uploads.Close()
	}
	if indexer != nil {
		indexer.Close()
	}
	if previews != nil {
		previews.Close()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
}

func whisperConfigured(cfg config.Config) bool {
	_, err := os.Stat(cfg.WhisperScript)
	return err == nil
}
