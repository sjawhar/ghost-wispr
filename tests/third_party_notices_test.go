package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// verbatimTexts are the upstream licence and notice files the release binary's
// statically linked code requires to travel with it, each with the sha256 of
// its text as the pinned upstream ref holds it (trailing newlines stripped).
// THIRD_PARTY_NOTICES.md, which the release ships beside the binary, must
// carry each one whole (review 5998292452 of #123: a link is no copy).
var verbatimTexts = map[string]string{
	// The Apache License 2.0, as apache.org serves it: sherpa-onnx, kaldi-native-fbank,
	// kaldi-decoder, kaldifst, simple-sentencepiece, OpenFst and the model carry it, and its
	// section 4(a) requires a copy with every distribution.
	"Apache-2.0": "58d1e17ffe5109a7ae296caafcadfdbe6a7d176f0bc4ab01e12a689b0499d8bd",
	// OpenFst's COPYING and AUTHORS.
	"openfst-COPYING": "f3d3d73a5e05d4c785ae91e83f22838c5939a371a7a88f0b1a284aa05aab59d8",
	"openfst-AUTHORS": "2d58d38ed4eb8ff1a8f03c3e0220e7d291d5c948930b5d4931d3a37a798cc2dc",
	// ONNX Runtime 1.28.2: its licence and the notices of the code it bundles.
	"onnxruntime-LICENSE":           "e9861036987ef519430f50a43532184b9fa0889cb6b3a0102acf7651181e8575",
	"onnxruntime-ThirdPartyNotices": "c4fcfa1b48ff5d5b8b269a7d016e491c029d737f362e4d402947cf2514008e5d",
	// KISS FFT, utf8proc, nlohmann/json, hclust-cpp and Eigen.
	"kissfft-COPYING":       "58a224bdc3eb54bb9e647f70e1732318541986e02fda4e407ce28b80b4d53d49",
	"kissfft-BSD-3-Clause":  "102aa9140e199ab8398adc37145c895238ed40b40bb1c67e04a1ae97983a2af9",
	"utf8proc-LICENSE.md":   "8553949f3b99cf0ebc6febd1c9abff6737b8474fd06e1cb02003cafa69a3a80d",
	"json-LICENSE.MIT":      "dd262584b550d76b855b86d77c2e522b02e397a0133dc5840e8db112ea681cb8",
	"hclust-cpp-LICENSE":    "dfce78b3108e5c75f4e2550c8299a9d2979d74f15c7a8c2c0239b4f87e5aaef6",
	"eigen-COPYING.MPL2":    "c76f740d1521b9bed9ca7a04ad526c310493c62621b1341d623b431736533b30",
	"eigen-COPYING.README":  "96b71a99a93109c7e49c24b6eea553af05ecdbea4194155c166c7e7d5a23497d",
	"eigen-COPYING.BSD":     "4f877e5ae4672568ef82cfd0023e2cef4a7cf55d867ab249efc9569a7eb9e5b1",
	"eigen-COPYING.MINPACK": "3c7b996f137f46767e318a26629216c7287467e419c9b5113c82727e721a7d38",
}

// verbatimBlock is one whole upstream text in THIRD_PARTY_NOTICES.md: a
// marker naming it, then the text in a ~~~~ fence.
var verbatimBlock = regexp.MustCompile("(?s)<!-- verbatim: ([A-Za-z0-9.+_-]+) -->\n~~~~text\n(.*?)\n~~~~\n")

// TestTheReleaseNoticesCarryEveryLicenceWhole fails if THIRD_PARTY_NOTICES.md
// lacks any upstream licence or notice text whole, or holds one that differs
// from the pinned upstream text, or names no row for a library the listener
// links, or if the release does not ship it.
func TestTheReleaseNoticesCarryEveryLicenceWhole(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_NOTICES.md"))
	if err != nil {
		t.Fatal(err)
	}
	notices := string(data)
	found := map[string]bool{}
	for _, m := range verbatimBlock.FindAllStringSubmatch(notices, -1) {
		name, text := m[1], strings.TrimRight(m[2], "\n")
		want, known := verbatimTexts[name]
		if !known {
			t.Errorf("THIRD_PARTY_NOTICES.md holds a verbatim text %q that this test does not pin", name)
			continue
		}
		sum := sha256.Sum256([]byte(text))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("THIRD_PARTY_NOTICES.md's %s has sha256 %s, want the upstream text's %s", name, got, want)
		}
		found[name] = true
	}
	for name := range verbatimTexts {
		if !found[name] {
			t.Errorf("THIRD_PARTY_NOTICES.md lacks the whole text of %s", name)
		}
	}

	// Every library the listener links statically has a row.
	engine, err := os.ReadFile(filepath.Join(root, "internal", "wake", "local", "engine_kws.go"))
	if err != nil {
		t.Fatal(err)
	}
	system := map[string]bool{"stdc++": true, "m": true, "dl": true, "pthread": true}
	linked := regexp.MustCompile(`-l([A-Za-z0-9_+-]+)`).FindAllStringSubmatch(string(engine), -1)
	if len(linked) < 5 {
		t.Fatalf("UNKNOWN: read %d -l flags in engine_kws.go", len(linked))
	}
	for _, l := range linked {
		if !system[l[1]] && !strings.Contains(notices, "`lib"+l[1]+"`") {
			t.Errorf("THIRD_PARTY_NOTICES.md names no row for the linked library lib%s", l[1])
		}
	}

	release, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(release), "THIRD_PARTY_NOTICES.md") < 2 {
		t.Error("release.yml neither uploads nor publishes THIRD_PARTY_NOTICES.md beside the binary")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("UNKNOWN: %s is not the repository root: %v", root, err)
	}
	return root
}
