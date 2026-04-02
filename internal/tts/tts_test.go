package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func fakeMP3() []byte {
	// Minimal bytes to simulate non-empty audio payload.
	return []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00, 0x01, 0x02}
}

// ---------------------------------------------------------------------------
// validateText
// ---------------------------------------------------------------------------

func TestValidateText_Empty(t *testing.T) {
	if err := validateText("", 100); err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestValidateText_TooLong(t *testing.T) {
	err := validateText(strings.Repeat("a", 101), 100)
	if err == nil {
		t.Fatal("expected error for text exceeding max length")
	}
	var tooLong *ErrTextTooLong
	if ok := isErrTextTooLong(err, &tooLong); !ok {
		t.Fatalf("expected ErrTextTooLong, got %T", err)
	}
	if tooLong.Length != 101 || tooLong.MaxLength != 100 {
		t.Fatalf("unexpected lengths: got %d/%d", tooLong.Length, tooLong.MaxLength)
	}
}

func TestValidateText_OK(t *testing.T) {
	if err := validateText("hello world", 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// isErrTextTooLong unwraps through fmt.Errorf %w wrapping.
func isErrTextTooLong(err error, target **ErrTextTooLong) bool {
	for err != nil {
		if e, ok := err.(*ErrTextTooLong); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---------------------------------------------------------------------------
// ElevenLabs
// ---------------------------------------------------------------------------

func TestElevenLabs_Synthesize_Success(t *testing.T) {
	mp3 := fakeMP3()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/text-to-speech/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("xi-api-key") != "test-key" {
			t.Errorf("missing or wrong api key header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content type: %s", r.Header.Get("Content-Type"))
		}

		var body elevenLabsRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Text != "Hello world" {
			t.Errorf("unexpected text: %q", body.Text)
		}

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(mp3)
	}))
	defer srv.Close()

	p, err := NewElevenLabsProvider("test-key", "voice-1", WithElevenLabsBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := p.Synthesize(context.Background(), SpeechRequest{Text: "Hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Audio) != len(mp3) {
		t.Fatalf("audio length: got %d, want %d", len(resp.Audio), len(mp3))
	}
	if resp.Format.Encoding != audio.EncodingMP3 {
		t.Errorf("expected MP3 encoding, got %v", resp.Format.Encoding)
	}
	if resp.Format.SampleRate != 44100 {
		t.Errorf("expected sample rate 44100, got %d", resp.Format.SampleRate)
	}
}

func TestElevenLabs_Synthesize_UsesDefaultVoice(t *testing.T) {
	var requestedVoice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		requestedVoice = parts[len(parts)-1]
		w.Write(fakeMP3())
	}))
	defer srv.Close()

	p, err := NewElevenLabsProvider("test-key", "my-default-voice", WithElevenLabsBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if requestedVoice != "my-default-voice" {
		t.Errorf("expected default voice, got %q", requestedVoice)
	}
}

func TestElevenLabs_Synthesize_OverridesVoice(t *testing.T) {
	var requestedVoice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		requestedVoice = parts[len(parts)-1]
		w.Write(fakeMP3())
	}))
	defer srv.Close()

	p, err := NewElevenLabsProvider("test-key", "default", WithElevenLabsBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test", Voice: "custom-voice"})
	if err != nil {
		t.Fatal(err)
	}
	if requestedVoice != "custom-voice" {
		t.Errorf("expected custom-voice, got %q", requestedVoice)
	}
}

func TestElevenLabs_Synthesize_TextTooLong(t *testing.T) {
	p, err := NewElevenLabsProvider("test-key", "", WithElevenLabsMaxTextLength(10))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: strings.Repeat("a", 11)})
	if err == nil {
		t.Fatal("expected error for text too long")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestElevenLabs_Synthesize_EmptyText(t *testing.T) {
	p, err := NewElevenLabsProvider("test-key", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: ""})
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestElevenLabs_Synthesize_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"detail": map[string]any{
				"status":  "bad_request",
				"message": "invalid voice_id",
			},
		})
	}))
	defer srv.Close()

	p, err := NewElevenLabsProvider("test-key", "bad-voice", WithElevenLabsBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid voice_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestElevenLabs_Synthesize_RateLimitRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(fakeMP3())
	}))
	defer srv.Close()

	p, err := NewElevenLabsProvider("test-key", "voice", WithElevenLabsBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := p.Synthesize(context.Background(), SpeechRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if len(resp.Audio) == 0 {
		t.Error("empty audio after retry")
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestElevenLabs_Synthesize_EmptyAudioResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write nothing — empty body.
	}))
	defer srv.Close()

	p, err := NewElevenLabsProvider("test-key", "voice", WithElevenLabsBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error for empty audio response")
	}
	if !strings.Contains(err.Error(), "empty audio") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestElevenLabs_NewProvider_NoAPIKey(t *testing.T) {
	_, err := NewElevenLabsProvider("", "")
	if err == nil {
		t.Fatal("expected error for missing api key")
	}
}

func TestElevenLabs_Name(t *testing.T) {
	p, _ := NewElevenLabsProvider("key", "")
	if p.Name() != "elevenlabs" {
		t.Errorf("expected 'elevenlabs', got %q", p.Name())
	}
}

// ---------------------------------------------------------------------------
// Google Cloud TTS
// ---------------------------------------------------------------------------

func TestGoogle_Synthesize_Success(t *testing.T) {
	mp3 := fakeMP3()
	b64Audio := base64.StdEncoding.EncodeToString(mp3)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/text:synthesize" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Goog-Api-Key") != "test-key" {
			t.Errorf("missing or wrong api key header")
		}

		var body googleTTSRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Input.Text != "Hello world" {
			t.Errorf("unexpected text: %q", body.Input.Text)
		}
		if body.Voice.LanguageCode != "en-US" {
			t.Errorf("unexpected language: %q", body.Voice.LanguageCode)
		}
		if body.AudioConfig.AudioEncoding != "MP3" {
			t.Errorf("unexpected encoding: %q", body.AudioConfig.AudioEncoding)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64Audio})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "en-US-Neural2-F", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := p.Synthesize(context.Background(), SpeechRequest{Text: "Hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Audio) != len(mp3) {
		t.Fatalf("audio length: got %d, want %d", len(resp.Audio), len(mp3))
	}
	if resp.Format.Encoding != audio.EncodingMP3 {
		t.Errorf("expected MP3 encoding, got %v", resp.Format.Encoding)
	}
	if resp.Format.SampleRate != 24000 {
		t.Errorf("expected sample rate 24000, got %d", resp.Format.SampleRate)
	}
}

func TestGoogle_Synthesize_UsesDefaultVoice(t *testing.T) {
	var requestedVoice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body googleTTSRequest
		json.NewDecoder(r.Body).Decode(&body)
		requestedVoice = body.Voice.Name
		b64 := base64.StdEncoding.EncodeToString(fakeMP3())
		json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "my-voice", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if requestedVoice != "my-voice" {
		t.Errorf("expected default voice 'my-voice', got %q", requestedVoice)
	}
}

func TestGoogle_Synthesize_OverridesVoiceAndLanguage(t *testing.T) {
	var reqVoice, reqLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body googleTTSRequest
		json.NewDecoder(r.Body).Decode(&body)
		reqVoice = body.Voice.Name
		reqLang = body.Voice.LanguageCode
		b64 := base64.StdEncoding.EncodeToString(fakeMP3())
		json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "default-voice", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test", Voice: "custom-voice", Language: "de-DE"})
	if err != nil {
		t.Fatal(err)
	}
	if reqVoice != "custom-voice" {
		t.Errorf("expected custom-voice, got %q", reqVoice)
	}
	if reqLang != "de-DE" {
		t.Errorf("expected de-DE, got %q", reqLang)
	}
}

func TestGoogle_Synthesize_TextTooLong(t *testing.T) {
	p, err := NewGoogleProvider("test-key", "", WithGoogleMaxTextLength(10))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: strings.Repeat("a", 11)})
	if err == nil {
		t.Fatal("expected error for text too long")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGoogle_Synthesize_EmptyText(t *testing.T) {
	p, err := NewGoogleProvider("test-key", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: ""})
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestGoogle_Synthesize_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    403,
				"message": "permission denied",
				"status":  "PERMISSION_DENIED",
			},
		})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "voice", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGoogle_Synthesize_EmptyAudioContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: ""})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "voice", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error for empty audio content")
	}
	if !strings.Contains(err.Error(), "empty audioContent") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGoogle_Synthesize_InvalidBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: "not-valid-base64!!!"})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "voice", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Synthesize(context.Background(), SpeechRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
	if !strings.Contains(err.Error(), "decode audio content") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGoogle_Synthesize_RateLimitRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		b64 := base64.StdEncoding.EncodeToString(fakeMP3())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64})
	}))
	defer srv.Close()

	p, err := NewGoogleProvider("test-key", "voice", WithGoogleBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := p.Synthesize(context.Background(), SpeechRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if len(resp.Audio) == 0 {
		t.Error("empty audio after retry")
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestGoogle_NewProvider_NoAPIKey(t *testing.T) {
	_, err := NewGoogleProvider("", "")
	if err == nil {
		t.Fatal("expected error for missing api key")
	}
}

func TestGoogle_Name(t *testing.T) {
	p, _ := NewGoogleProvider("key", "")
	if p.Name() != "google" {
		t.Errorf("expected 'google', got %q", p.Name())
	}
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

var _ Provider = (*ElevenLabsProvider)(nil)
var _ Provider = (*GoogleProvider)(nil)
