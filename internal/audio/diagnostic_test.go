package audio

import (
	"testing"
)

func TestAnalyzePCM_SilentAudio(t *testing.T) {
	samples := make([]int16, 16000) // 1 second at 16kHz, all zeros
	report := AnalyzePCM(samples, 16000)

	if report.SampleRate != 16000 {
		t.Errorf("expected sample rate 16000, got %d", report.SampleRate)
	}
	if report.DurationMs != 1000 {
		t.Errorf("expected duration 1000ms, got %d", report.DurationMs)
	}
	if report.AmplitudeAvg != 0 {
		t.Errorf("expected zero amplitude avg for silence, got %f", report.AmplitudeAvg)
	}
	if report.AmplitudePeak != 0 {
		t.Errorf("expected zero peak for silence, got %f", report.AmplitudePeak)
	}
	if report.ClippingCount != 0 {
		t.Errorf("expected no clipping for silence, got %d", report.ClippingCount)
	}
}

func TestAnalyzePCM_LoudAudio(t *testing.T) {
	samples := make([]int16, 16000)
	for i := range samples {
		samples[i] = 32700 // near max, triggers clipping
	}
	report := AnalyzePCM(samples, 16000)

	if report.ClippingCount != 16000 {
		t.Errorf("expected all samples clipping, got %d", report.ClippingCount)
	}
	if report.AmplitudePeak < 0.99 {
		t.Errorf("expected peak near 1.0, got %f", report.AmplitudePeak)
	}
	// Should recommend reducing volume
	found := false
	for _, r := range report.Recommendations {
		if r == "Clipping detected — reduce mic input volume" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected clipping recommendation, got %v", report.Recommendations)
	}
}

func TestAnalyzePCM_NormalAudio(t *testing.T) {
	samples := make([]int16, 16000)
	for i := range samples {
		// Simulate moderate audio: alternating values around 5000
		if i%2 == 0 {
			samples[i] = 5000
		} else {
			samples[i] = -5000
		}
	}
	report := AnalyzePCM(samples, 16000)

	if report.AmplitudeAvg < 0.1 || report.AmplitudeAvg > 0.2 {
		t.Errorf("expected moderate amplitude avg, got %f", report.AmplitudeAvg)
	}
	if report.ClippingCount != 0 {
		t.Errorf("expected no clipping, got %d", report.ClippingCount)
	}
	if report.DurationMs != 1000 {
		t.Errorf("expected 1000ms duration, got %d", report.DurationMs)
	}
}

func TestAnalyzePCM_Recommendations_LowLevel(t *testing.T) {
	samples := make([]int16, 16000)
	for i := range samples {
		samples[i] = 10 // very low level
	}
	report := AnalyzePCM(samples, 16000)

	foundLow := false
	foundPeak := false
	for _, r := range report.Recommendations {
		if r == "Audio level very low — check mic gain or distance" {
			foundLow = true
		}
		if r == "Peak level too low — increase mic input volume" {
			foundPeak = true
		}
	}
	if !foundLow {
		t.Errorf("expected low level recommendation, got %v", report.Recommendations)
	}
	if !foundPeak {
		t.Errorf("expected low peak recommendation, got %v", report.Recommendations)
	}
}

func TestAnalyzePCM_EmptySamples(t *testing.T) {
	report := AnalyzePCM(nil, 16000)

	if report.DurationMs != 0 {
		t.Errorf("expected 0 duration for empty samples, got %d", report.DurationMs)
	}
	if len(report.Recommendations) == 0 {
		t.Error("expected at least one recommendation for empty samples")
	}
}

func TestAnalyzePCM_ZeroSampleRate(t *testing.T) {
	report := AnalyzePCM([]int16{100, 200}, 0)
	if len(report.Recommendations) == 0 {
		t.Error("expected recommendation for zero sample rate")
	}
}
