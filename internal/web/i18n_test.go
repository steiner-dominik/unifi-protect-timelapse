package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every language must define exactly the same keys, or switching language would
// silently fall back to raw key names for whatever is missing.
func TestTranslationsHaveIdenticalKeys(t *testing.T) {
	entries, err := fs.ReadDir(assets, "assets/i18n")
	if err != nil {
		t.Fatalf("reading translations: %v", err)
	}

	keysByLang := make(map[string][]string)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := fs.ReadFile(assets, "assets/i18n/"+entry.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s is not valid JSON: %v", entry.Name(), err)
		}

		keys := flattenKeys(parsed, "")
		slices.Sort(keys)
		keysByLang[strings.TrimSuffix(entry.Name(), ".json")] = keys
	}

	if len(keysByLang) < 2 {
		t.Fatalf("expected at least two languages, found %d", len(keysByLang))
	}

	reference := keysByLang["en"]
	if reference == nil {
		t.Fatal("en.json is missing")
	}
	for lang, keys := range keysByLang {
		if lang == "en" {
			continue
		}
		for _, key := range reference {
			if !slices.Contains(keys, key) {
				t.Errorf("%s.json is missing key %q", lang, key)
			}
		}
		for _, key := range keys {
			if !slices.Contains(reference, key) {
				t.Errorf("%s.json has key %q that en.json does not", lang, key)
			}
		}
	}
}

// Every key the markup asks for must exist, or the UI renders dotted key names.
func TestMarkupKeysExistInEnglish(t *testing.T) {
	markup, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	raw, err := fs.ReadFile(assets, "assets/i18n/en.json")
	if err != nil {
		t.Fatalf("reading en.json: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("en.json: %v", err)
	}
	available := flattenKeys(parsed, "")

	for _, attr := range []string{"data-i18n=", "data-i18n-aria=", "data-i18n-alt="} {
		for _, key := range attributeValues(string(markup), attr) {
			if !slices.Contains(available, key) {
				t.Errorf("index.html references %s%q which en.json does not define", attr, key)
			}
		}
	}
}

func flattenKeys(node map[string]any, prefix string) []string {
	var keys []string
	for key, value := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			keys = append(keys, flattenKeys(nested, path)...)
			continue
		}
		keys = append(keys, path)
	}
	return keys
}

func attributeValues(markup, attr string) []string {
	var values []string
	for rest := markup; ; {
		idx := strings.Index(rest, attr)
		if idx < 0 {
			return values
		}
		rest = rest[idx+len(attr):]
		if !strings.HasPrefix(rest, `"`) {
			continue
		}
		rest = rest[1:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			return values
		}
		values = append(values, rest[:end])
		rest = rest[end:]
	}
}

func TestDayReportEndpoint(t *testing.T) {
	server, cfg := newTestServer(t, "")
	writeTestFrames(t, cfg.Sync.ArchiveDir, "2026-09-07",
		"snapshot_2026-09-07-05-00-00.jpg",
		"snapshot_2026-09-07-05-05-00.jpg",
		"snapshot_2026-09-07-09-05-00.jpg")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/api/archive/days/2026-09-07/report", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("report = %d", recorder.Code)
	}

	var report struct {
		Frames        int `json:"frames"`
		MissingFrames int `json:"missingFrames"`
		Gaps          []struct {
			After  string `json:"after"`
			Before string `json:"before"`
		} `json:"gaps"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	if report.Frames != 3 {
		t.Errorf("Frames = %d, want 3", report.Frames)
	}
	if len(report.Gaps) != 1 {
		t.Fatalf("expected 1 gap, got %d", len(report.Gaps))
	}
	if report.Gaps[0].After != "05:05:00" || report.Gaps[0].Before != "09:05:00" {
		t.Errorf("unexpected gap boundaries: %+v", report.Gaps[0])
	}
}

func TestZipDownloadContainsEveryFrame(t *testing.T) {
	server, cfg := newTestServer(t, "")
	names := []string{
		"snapshot_2026-09-07-05-00-00.jpg",
		"snapshot_2026-09-07-05-05-00.jpg",
	}
	writeTestFrames(t, cfg.Sync.ArchiveDir, "2026-09-07", names...)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/api/archive/days/2026-09-07/download.zip", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("zip = %d", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := recorder.Header().Get("Content-Disposition"); !strings.Contains(cd, "2026-09-07.zip") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	body := recorder.Body.Bytes()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the response is not a readable zip: %v", err)
	}
	if len(reader.File) != len(names) {
		t.Fatalf("zip holds %d entries, want %d", len(reader.File), len(names))
	}
	for i, file := range reader.File {
		if file.Name != names[i] {
			t.Errorf("entry %d is %q, want %q", i, file.Name, names[i])
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", file.Name, err)
		}
		content, _ := io.ReadAll(rc)
		_ = rc.Close()
		if len(content) == 0 {
			t.Errorf("%s is empty in the zip", file.Name)
		}
	}
}

// ffmpeg is deliberately not present in the test binary's environment, so the
// route must not be registered at all.
func TestVideoRouteAbsentWithoutFFmpeg(t *testing.T) {
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/api/archive/days/2026-09-07/video.mp4", nil))

	if recorder.Code == http.StatusOK {
		t.Error("video rendering should not be offered when ffmpeg is unavailable")
	}
}

// With capture disabled there is no mirrored image, so the live view falls back
// to the newest archived frame. This is what makes the watchdog deployment show
// something useful.
func TestLatestFallsBackToTheArchive(t *testing.T) {
	server, cfg := newTestServer(t, "")
	cfg.Capture.Enabled = false
	writeTestFrames(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-05-00-00.jpg")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/latest.jpg", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected the archived frame to stand in, got %d", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func writeTestFrames(t *testing.T, root, day string, names ...string) {
	t.Helper()
	dir := filepath.Join(root, day[:4], day[:7], day)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte{0xFF, 0xD8, 0xFF, 0x00}, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}
