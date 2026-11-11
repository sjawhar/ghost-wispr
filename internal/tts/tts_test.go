package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func fakeMP3() []byte {
	// Minimal bytes to simulate non-empty audio payload.
	return []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00, 0x01, 0x02}
}

func fakeWAV(pcm []byte) []byte {
	wav := make([]byte, 44+len(pcm))
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+len(pcm)))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], 1)
	binary.LittleEndian.PutUint32(wav[24:28], 24000)
	binary.LittleEndian.PutUint32(wav[28:32], 48000)
	binary.LittleEndian.PutUint16(wav[32:34], 2)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav
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
		_, _ = w.Write(mp3)
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
		_, _ = w.Write(fakeMP3())
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
		_, _ = w.Write(fakeMP3())
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		_, _ = w.Write(fakeMP3())
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

func TestTTSUsesAuthCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fake-tts-token" {
			t.Errorf("Authorization = %q, want fake-tts-token", got)
		}
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: base64.StdEncoding.EncodeToString(fakeMP3())})
	}))
	t.Cleanup(server.Close)

	credentialPath := googleauthtest.X509CredentialFile(t, "fake-tts-token")
	credentials, err := googleauth.Detect(context.Background(), credentialPath, googleauth.CloudPlatformScope)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	provider, err := NewGoogleProvider("", "en-US-Chirp3-HD-Charon", WithAuthCredentials(credentials), WithGoogleBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewGoogleProvider: %v", err)
	}
	if _, err := provider.Synthesize(context.Background(), SpeechRequest{Text: "Hello"}); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
}

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
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64Audio})
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
		_ = json.NewDecoder(r.Body).Decode(&body)
		requestedVoice = body.Voice.Name
		b64 := base64.StdEncoding.EncodeToString(fakeMP3())
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64})
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
		_ = json.NewDecoder(r.Body).Decode(&body)
		reqVoice = body.Voice.Name
		reqLang = body.Voice.LanguageCode
		b64 := base64.StdEncoding.EncodeToString(fakeMP3())
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64})
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: ""})
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
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: "not-valid-base64!!!"})
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
		_ = json.NewEncoder(w).Encode(googleTTSResponse{AudioContent: b64})
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
// Gemini Vertex TTS
// ---------------------------------------------------------------------------

func TestGemini_Synthesize_SendsConfiguredVertexRequest(t *testing.T) {
	const (
		project = "theorem-agent-infra"
		model   = "gemini-3.8-flash-tts"
		voice   = "Kore"
		token   = "configured-vertex-token"
	)
	pcm := []byte{0x01, 0x00, 0x02, 0x00}
	wav := fakeWAV(pcm)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got, want := r.URL.Path, "/v1/projects/"+project+"/locations/global/publishers/google/models/"+model+":generateContent"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}

		var body struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
			GenerationConfig struct {
				ResponseModalities []string `json:"responseModalities"`
				SpeechConfig       struct {
					VoiceConfig struct {
						Voice string `json:"voice"`
					} `json:"voiceConfig"`
				} `json:"speechConfig"`
			} `json:"generationConfig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		} else {
			if len(body.Contents) != 1 || body.Contents[0].Role != "user" || len(body.Contents[0].Parts) != 1 || body.Contents[0].Parts[0].Text != "Speak this." {
				t.Errorf("contents = %#v, want the requested text", body.Contents)
			}
			if len(body.GenerationConfig.ResponseModalities) != 1 || body.GenerationConfig.ResponseModalities[0] != "AUDIO" {
				t.Errorf("responseModalities = %#v, want [AUDIO]", body.GenerationConfig.ResponseModalities)
			}
			if got := body.GenerationConfig.SpeechConfig.VoiceConfig.Voice; got != voice {
				t.Errorf("voice = %q, want %q", got, voice)
			}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{
						"inlineData": map[string]string{
							"mimeType": "audio/wav",
							"data":     base64.StdEncoding.EncodeToString(wav),
						},
					}},
				},
			}},
		})
	}))
	t.Cleanup(server.Close)

	credentialPath := googleauthtest.X509CredentialFile(t, token)
	credentials, err := googleauth.Detect(context.Background(), credentialPath, googleauth.CloudPlatformScope)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	provider, err := NewGeminiProvider(project, model, voice, WithGeminiAuthCredentials(credentials), WithGeminiBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewGeminiProvider: %v", err)
	}

	response, err := provider.Synthesize(context.Background(), SpeechRequest{Text: "Speak this."})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !bytes.Equal(response.Audio, pcm) {
		t.Errorf("audio = %v, want %v", response.Audio, pcm)
	}
	if got, want := response.Format, (audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); got != want {
		t.Errorf("format = %#v, want %#v", got, want)
	}
}

func TestDecodeGeminiWAVRejectsNonPCM16(t *testing.T) {
	wav := fakeWAV([]byte{0x01, 0x00})
	binary.LittleEndian.PutUint16(wav[20:22], 3)

	if _, _, err := decodeGeminiWAV(wav); err == nil || !strings.Contains(err.Error(), "16-bit PCM") {
		t.Fatalf("decodeGeminiWAV error = %v, want unsupported audio encoding", err)
	}
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

var _ Provider = (*ElevenLabsProvider)(nil)
var _ Provider = (*GoogleProvider)(nil)
var _ Provider = (*GeminiProvider)(nil)
