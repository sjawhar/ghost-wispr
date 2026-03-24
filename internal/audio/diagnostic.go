package audio

import (
	"math"
	"sort"
)

// DiagnosticReport contains mic audio quality analysis results.
type DiagnosticReport struct {
	SampleRate      int      `json:"sample_rate"`
	DurationMs      int      `json:"duration_ms"`
	AmplitudeAvg    float64  `json:"amplitude_avg"`
	AmplitudePeak   float64  `json:"amplitude_peak"`
	NoiseFloor      float64  `json:"noise_floor"`
	ClippingCount   int      `json:"clipping_count"`
	SNREstimateDB   float64  `json:"snr_estimate_db"`
	Recommendations []string `json:"recommendations"`
}

// AnalyzePCM analyzes raw PCM audio samples (int16 format) and returns a diagnostic report
// with amplitude metrics, noise floor estimate, clipping detection, and recommendations.
func AnalyzePCM(samples []int16, sampleRate int) DiagnosticReport {
	report := DiagnosticReport{
		SampleRate:      sampleRate,
		Recommendations: []string{},
	}

	if len(samples) == 0 || sampleRate <= 0 {
		report.Recommendations = append(report.Recommendations, "No audio samples to analyze")
		return report
	}

	report.DurationMs = len(samples) * 1000 / sampleRate

	// Compute absolute values for analysis.
	absValues := make([]float64, len(samples))
	var sumAbs float64
	var peak float64
	clipping := 0

	for i, s := range samples {
		abs := math.Abs(float64(s))
		absValues[i] = abs
		sumAbs += abs
		if abs > peak {
			peak = abs
		}
		if abs >= 32700 {
			clipping++
		}
	}

	const maxVal = 32768.0
	report.AmplitudeAvg = (sumAbs / float64(len(samples))) / maxVal
	report.AmplitudePeak = peak / maxVal
	report.ClippingCount = clipping

	// Noise floor: 10th percentile of absolute values.
	sorted := make([]float64, len(absValues))
	copy(sorted, absValues)
	sort.Float64s(sorted)
	idx := len(sorted) / 10
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	report.NoiseFloor = sorted[idx] / maxVal

	// SNR estimate.
	if report.NoiseFloor > 0 {
		report.SNREstimateDB = 20 * math.Log10(report.AmplitudePeak/report.NoiseFloor)
	}

	// Generate recommendations.
	if report.AmplitudeAvg < 0.01 {
		report.Recommendations = append(report.Recommendations, "Audio level very low — check mic gain or distance")
	}
	if report.AmplitudePeak < 0.05 {
		report.Recommendations = append(report.Recommendations, "Peak level too low — increase mic input volume")
	}
	if report.ClippingCount > 10 {
		report.Recommendations = append(report.Recommendations, "Clipping detected — reduce mic input volume")
	}
	if report.NoiseFloor > 0.1 {
		report.Recommendations = append(report.Recommendations, "High noise floor — check for background noise or interference")
	}
	if report.SNREstimateDB > 0 && report.SNREstimateDB < 10 {
		report.Recommendations = append(report.Recommendations, "Poor signal-to-noise ratio — consider a better microphone or quieter environment")
	}
	if len(report.Recommendations) == 0 {
		report.Recommendations = append(report.Recommendations, "Audio quality looks good")
	}

	return report
}
