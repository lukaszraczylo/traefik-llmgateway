package traefikllmgateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// geminiImagesGoldenTestdataDir is where every fixture this file walks
// lives.
const geminiImagesGoldenTestdataDir = "testdata/gemini_images"

// geminiImagesGoldenCreated is the fixed "created" timestamp every
// response golden fixture's "_want.json" bakes in —
// openAIImagesResponseFromGemini takes it as a caller-supplied parameter
// (Imagen's response carries none), so the golden test must supply the
// same fixed value the "_want.json" files were written against.
const geminiImagesGoldenCreated = int64(1734000000)

// geminiImagesReadGoldenJSON reads and json.Unmarshals name (relative to
// testdata/gemini_images) into a fresh map[string]any.
func geminiImagesReadGoldenJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(geminiImagesGoldenTestdataDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return m
}

// TestGeminiImagesRequestGolden walks every "req_*_in.json" fixture under
// testdata/gemini_images, translating it via geminiImagesRequestFromOpenAI,
// and asserts the result against the sibling "_want.json" fixture (a
// successful translation) or "_wanterr.json" fixture (a *translateError),
// whichever is present — exactly one of the two exists per fixture.
func TestGeminiImagesRequestGolden(t *testing.T) {
	entries, err := os.ReadDir(geminiImagesGoldenTestdataDir)
	if err != nil {
		t.Fatalf("read testdata dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), "_in.json"); ok && strings.HasPrefix(n, "req_") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no req_*_in.json fixtures found under %s", geminiImagesGoldenTestdataDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			in := geminiImagesReadGoldenJSON(t, name+"_in.json")

			got, err := geminiImagesRequestFromOpenAI(in)

			wantErrPath := filepath.Join(geminiImagesGoldenTestdataDir, name+"_wanterr.json")
			if _, statErr := os.Stat(wantErrPath); statErr == nil {
				var want wantErrFixture
				b, rerr := os.ReadFile(wantErrPath)
				if rerr != nil {
					t.Fatalf("read %s: %v", wantErrPath, rerr)
				}
				if uerr := json.Unmarshal(b, &want); uerr != nil {
					t.Fatalf("decode %s: %v", wantErrPath, uerr)
				}
				assertTranslateError(t, err, want)
				return
			}

			if err != nil {
				t.Fatalf("geminiImagesRequestFromOpenAI: %v", err)
			}
			want := geminiImagesReadGoldenJSON(t, name+"_want.json")
			gotN := normalizeJSON(t, got)
			wantN := normalizeJSON(t, want)
			if !reflect.DeepEqual(gotN, wantN) {
				gotB, _ := json.MarshalIndent(gotN, "", "  ")
				wantB, _ := json.MarshalIndent(wantN, "", "  ")
				t.Errorf("geminiImagesRequestFromOpenAI(%s) mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
			}
		})
	}
}

// TestGeminiImagesResponseGolden walks every "resp_*_in.json" fixture,
// translating it via openAIImagesResponseFromGemini with the fixed
// geminiImagesGoldenCreated, and asserts the result against the sibling
// "_want.json" fixture.
func TestGeminiImagesResponseGolden(t *testing.T) {
	entries, err := os.ReadDir(geminiImagesGoldenTestdataDir)
	if err != nil {
		t.Fatalf("read testdata dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), "_in.json"); ok && strings.HasPrefix(n, "resp_") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no resp_*_in.json fixtures found under %s", geminiImagesGoldenTestdataDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(geminiImagesGoldenTestdataDir, name+"_in.json"))
			if err != nil {
				t.Fatalf("read %s_in.json: %v", name, err)
			}

			got, err := openAIImagesResponseFromGemini(body, geminiImagesGoldenCreated)
			if err != nil {
				t.Fatalf("openAIImagesResponseFromGemini: %v", err)
			}

			want := geminiImagesReadGoldenJSON(t, name+"_want.json")
			gotN := normalizeJSON(t, got)
			wantN := normalizeJSON(t, want)
			if !reflect.DeepEqual(gotN, wantN) {
				gotB, _ := json.MarshalIndent(gotN, "", "  ")
				wantB, _ := json.MarshalIndent(wantN, "", "  ")
				t.Errorf("openAIImagesResponseFromGemini(%s) mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
			}
		})
	}
}

// TestGeminiImageAspectRatio_UnknownSizeErrors pins the boundary the
// golden walker's req_size_unknown fixture already exercises end to end,
// directly against geminiImageAspectRatio: an unrecognized, non-empty
// size string is a *translateError, not a silent fallback to "1:1".
func TestGeminiImageAspectRatio_UnknownSizeErrors(t *testing.T) {
	_, err := geminiImageAspectRatio("640x480")
	var terr *translateError
	if err == nil {
		t.Fatal("geminiImageAspectRatio(\"640x480\") = nil error, want *translateError")
	}
	if te, ok := err.(*translateError); ok {
		terr = te
	} else {
		t.Fatalf("err = %v (%T), want *translateError", err, err)
	}
	if terr.notSupported {
		t.Error("notSupported = true, want false (this is a 400, not a 501)")
	}
}

// TestGeminiImagesRequest_CountBounds pins the accepted range for n: absent
// or non-positive defaults to 1, and more than maxImagesPerRequest is refused.
func TestGeminiImagesRequest_CountBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		req     map[string]any
		name    string
		want    int64
		wantErr bool
	}{
		{name: "absent defaults to one", req: map[string]any{"prompt": "p"}, want: 1},
		{name: "zero defaults to one", req: map[string]any{"prompt": "p", "n": 0.0}, want: 1},
		{name: "negative defaults to one", req: map[string]any{"prompt": "p", "n": -4.0}, want: 1},
		{name: "in range", req: map[string]any{"prompt": "p", "n": 4.0}, want: 4},
		{name: "at the cap", req: map[string]any{"prompt": "p", "n": float64(maxImagesPerRequest)}, want: maxImagesPerRequest},
		{name: "one over the cap", req: map[string]any{"prompt": "p", "n": float64(maxImagesPerRequest + 1)}, wantErr: true},
		{name: "huge", req: map[string]any{"prompt": "p", "n": 1e9}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := geminiImagesRequestFromOpenAI(tt.req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			params, _ := out["parameters"].(map[string]any)
			if got, _ := params["sampleCount"].(int64); got != tt.want {
				t.Errorf("sampleCount = %v, want %d", params["sampleCount"], tt.want)
			}
		})
	}
}
