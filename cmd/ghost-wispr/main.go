package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	microphone "github.com/deepgram/deepgram-go-sdk/v3/pkg/audio/microphone"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

//go:embed static/*
var staticFiles embed.FS

type recorderState struct {
	mic    *microphone.Microphone
	mu     sync.RWMutex
	paused bool
}

func (r *recorderState) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = true
	if r.mic != nil {
		r.mic.Mute()
	}
}

func (r *recorderState) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = false
	if r.mic != nil {
		r.mic.Unmute()
	}
}

func (r *recorderState) IsPaused() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.paused
}

func (r *recorderState) SetMic(mic *microphone.Microphone) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mic = mic
}

type transcriptCallback struct {
	manager session.LifecycleManager
}

func (c transcriptCallback) Message(mr *api.MessageResponse) error {
	if c.manager == nil {
		return nil
	}
	return c.manager.Message(mr)
}

func (c transcriptCallback) Open(*api.OpenResponse) error {
	log.Println("connected to Deepgram")
	return nil
}

func (c transcriptCallback) Metadata(*api.MetadataResponse) error { return nil }

func (c transcriptCallback) SpeechStarted(*api.SpeechStartedResponse) error { return nil }

func (c transcriptCallback) UtteranceEnd(ur *api.UtteranceEndResponse) error {
	if c.manager == nil {
		return nil
	}
	return c.manager.UtteranceEnd(ur)
}

func (c transcriptCallback) Close(*api.CloseResponse) error {
	log.Println("disconnected from Deepgram")
	return nil
}

func (c transcriptCallback) Error(er *api.ErrorResponse) error {
	log.Printf("deepgram error %s: %s", er.ErrCode, er.Description)
	return nil
}

func (c transcriptCallback) UnhandledEvent([]byte) error { return nil }

func main() {
	log.Println("ghost-wispr: starting")

	dbPath := envOrDefault("DB_PATH", "data/ghost-wispr.db")
	audioDir := envOrDefault("AUDIO_DIR", "data/audio")
	silenceTimeout := durationOrDefault("SILENCE_TIMEOUT", 30*time.Second)
	model := envOrDefault("OPENAI_MODEL", "gpt-4o-mini")

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		log.Fatalf("storage init failed: %v", err)
	}

	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static assets init failed: %v", err)
	}

	hub := server.NewHub()
	detector := session.NewDetector(silenceTimeout)
	audioRecorder := audio.NewRecorder(audioDir)

	var summarizer session.Summarizer
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		summarizer = summary.NewOpenAI(key, model, store)
	}

	manager := session.NewManager(store, audioRecorder, summarizer, hub, detector)

	recState := &recorderState{}

	handler, err := server.Handler(assets, hub, store, server.ControlHooks{
		Pause:    recState.Pause,
		Resume:   recState.Resume,
		IsPaused: recState.IsPaused,
		OnStatusChanged: func(paused bool) {
			hub.BroadcastStatusChanged(paused)
		},
	})
	if err != nil {
		log.Fatalf("build http handler failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = store.Close() }()

	httpServer := &http.Server{Addr: ":8080", Handler: handler}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server error: %v", err)
		}
	}()

	if folderID := os.Getenv("GDRIVE_FOLDER_ID"); folderID != "" {
		credPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
		syncer, syncErr := gdrive.NewSyncer(ctx, credPath, folderID)
		if syncErr != nil {
			log.Printf("warning: gdrive sync disabled: %v", syncErr)
		} else {
			go func() {
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						date := time.Now().UTC().Format("2006-01-02")
						if err := syncer.Sync(dbPath, date); err != nil {
							log.Printf("gdrive sync error: %v", err)
						}
					}
				}
			}()
		}
	}

	var mic *microphone.Microphone
	var dgWriter io.Writer
	var dgStop func()
	selectedSampleRate := 16000

	microphone.Initialize()
	defer microphone.Teardown()

	client.Init(client.InitLib{LogLevel: client.LogLevelDefault})

	for _, rate := range sampleRateCandidates() {
		mic, err = microphone.New(microphone.AudioConfig{InputChannels: 1, SamplingRate: float32(rate)})
		if err != nil {
			log.Printf("warning: microphone open failed at %d Hz: %v", rate, err)
			continue
		}
		selectedSampleRate = rate
		break
	}

	if mic == nil {
		log.Printf("warning: microphone unavailable, running API/UI only")
	} else {
		audioRecorder.SetSampleRate(selectedSampleRate)
		recState.SetMic(mic)
		if err := mic.Start(); err != nil {
			log.Printf("warning: microphone start failed at %d Hz, running API/UI only: %v", selectedSampleRate, err)
			mic = nil
			recState.SetMic(nil)
		} else {
			log.Printf("microphone started at %d Hz", selectedSampleRate)
		}
	}

	if mic != nil {
		cOptions := &interfaces.ClientOptions{EnableKeepAlive: true}
		tOptions := &interfaces.LiveTranscriptionOptions{
			Model:       "nova-2",
			Language:    "en-US",
			Diarize:     true,
			Punctuate:   true,
			SmartFormat: true,
			Encoding:    "linear16",
			SampleRate:  selectedSampleRate,
			Channels:    1,
		}

		dgClient, err := client.NewWSUsingCallback(ctx, "", cOptions, tOptions, transcriptCallback{manager: manager})
		if err != nil {
			log.Printf("warning: deepgram client unavailable, running API/UI only: %v", err)
		} else if ok := dgClient.Connect(); !ok {
			log.Printf("warning: deepgram connect failed, running API/UI only")
		} else {
			dgWriter = dgClient
			dgStop = func() {
				dgClient.Stop()
			}
			go func() {
				streamMicWithRetry(ctx, mic, audioRecorder.Writer(dgWriter), time.Sleep, log.Printf)
			}()
		}
	}

	log.Println("ghost-wispr: web UI on http://127.0.0.1:8080")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("ghost-wispr: shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := manager.ForceEndSession(shutdownCtx); err != nil {
		log.Printf("warning: force end session failed: %v", err)
	}

	if dgStop != nil {
		dgStop()
	}
	if mic != nil {
		_ = mic.Stop()
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("warning: http shutdown failed: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	return val
}

func durationOrDefault(key string, fallback time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid %s duration %q, using %s\n", key, val, fallback)
		return fallback
	}
	return d
}

func parseSampleRates(raw string) []int {
	parts := strings.Split(raw, ",")
	seen := make(map[int]struct{}, len(parts))
	result := make([]int, 0, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		rate, err := strconv.Atoi(trimmed)
		if err != nil || rate <= 0 {
			continue
		}
		if _, ok := seen[rate]; ok {
			continue
		}
		seen[rate] = struct{}{}
		result = append(result, rate)
	}

	return result
}

func sampleRateCandidates() []int {
	defaults := []int{16000, 48000, 44100, 32000, 24000}
	combined := make([]int, 0, 8)

	if preferred := strings.TrimSpace(os.Getenv("MIC_SAMPLE_RATE")); preferred != "" {
		combined = append(combined, parseSampleRates(preferred)...)
	}

	combined = append(combined, parseSampleRates(os.Getenv("MIC_SAMPLE_RATES"))...)
	combined = append(combined, defaults...)

	seen := make(map[int]struct{}, len(combined))
	result := make([]int, 0, len(combined))
	for _, rate := range combined {
		if _, ok := seen[rate]; ok {
			continue
		}
		seen[rate] = struct{}{}
		result = append(result, rate)
	}

	return result
}

type micStreamer interface {
	Stream(writer io.Writer) error
}

func streamMicWithRetry(
	ctx context.Context,
	streamer micStreamer,
	writer io.Writer,
	wait func(time.Duration),
	logf func(string, ...any),
) {
	for {
		if ctx.Err() != nil {
			return
		}

		err := streamer.Stream(writer)
		if err == nil || ctx.Err() != nil {
			return
		}

		if strings.Contains(strings.ToLower(err.Error()), "overflow") {
			logf("warning: mic input overflow, restarting stream")
			wait(250 * time.Millisecond)
			continue
		}

		logf("mic stream error: %v", err)
		return
	}
}
