package audio

const spliceSurvivorWindowSeconds = 10

// Survivors returns the unique tokens from segmentText that are absent from
// both neighbours but still appear within ten seconds of splice in words.
func Survivors(words []Word, splice float64, segmentText, before, after string) []string {
	neighbourTokens := make(map[string]struct{})
	for _, token := range normaliseText(before) {
		neighbourTokens[token] = struct{}{}
	}
	for _, token := range normaliseText(after) {
		neighbourTokens[token] = struct{}{}
	}

	segmentTokens := make(map[string]struct{})
	for _, token := range normaliseText(segmentText) {
		if _, shared := neighbourTokens[token]; !shared {
			segmentTokens[token] = struct{}{}
		}
	}
	if len(segmentTokens) == 0 {
		return nil
	}

	survived := make(map[string]struct{})
	lower := splice - spliceSurvivorWindowSeconds
	upper := splice + spliceSurvivorWindowSeconds
	for _, word := range words {
		if word.End < lower || word.Start > upper {
			continue
		}
		for _, token := range normaliseText(word.Text) {
			if _, inSegment := segmentTokens[token]; inSegment {
				survived[token] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(survived))
	for _, token := range normaliseText(segmentText) {
		if _, found := survived[token]; !found {
			continue
		}
		result = append(result, token)
		delete(survived, token)
	}
	return result
}
