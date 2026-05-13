package main

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

// makePNG returns the bytes of a w×h PNG.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// makeMapZIP builds an in-memory map ZIP with WHEELTEC.png, WHEELTEC.yaml, path.json.
func makeMapZIP(t *testing.T, pngBytes []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	write("WHEELTEC.png", pngBytes)
	write("WHEELTEC.yaml", []byte("resolution: 0.05\norigin: [-1.0, -2.0, 0.0]\n"))
	write("path.json", []byte(`{"point":{"0":{"name":"home","x":1.0,"y":2.0,"type":"home","angle":1.57},"1":{"name":"wp1","x":0,"y":0,"type":"wp","angle":0},"2":{"name":"C01","x":3.0,"y":4.0,"type":"station","angle":0}}}`))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestGetImageDimensions(t *testing.T) {
	t.Parallel()
	png := makePNG(t, 13, 7)
	w, h := getImageDimensions(png)
	if w != 13 || h != 7 {
		t.Errorf("getImageDimensions = %d×%d, want 13×7", w, h)
	}
	if w, h := getImageDimensions([]byte("short")); w != 0 || h != 0 {
		t.Errorf("getImageDimensions of garbage should be 0×0, got %d×%d", w, h)
	}
}

func TestExtractFromZIP(t *testing.T) {
	t.Parallel()
	pngBytes := makePNG(t, 20, 10)
	zipBytes := makeMapZIP(t, pngBytes)

	got, err := ExtractPNGFromZIP(zipBytes)
	if err != nil {
		t.Fatalf("ExtractPNGFromZIP: %v", err)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Errorf("ExtractPNGFromZIP returned different bytes")
	}

	meta, err := ExtractMetaFromZIP(zipBytes)
	if err != nil {
		t.Fatalf("ExtractMetaFromZIP: %v", err)
	}
	if meta.Resolution != 0.05 || meta.Origin[0] != -1.0 || meta.Origin[1] != -2.0 {
		t.Errorf("meta wrong: %+v", meta)
	}
	if meta.Width != 20 || meta.Height != 10 {
		t.Errorf("meta dimensions = %d×%d, want 20×10", meta.Width, meta.Height)
	}

	pois, err := ExtractPOIFromZIP(zipBytes)
	if err != nil {
		t.Fatalf("ExtractPOIFromZIP: %v", err)
	}
	// "wp" entry should be filtered → 2 POIs.
	if len(pois) != 2 {
		t.Fatalf("expected 2 POIs (wp filtered), got %d: %+v", len(pois), pois)
	}
	names := map[string]MapPOI{}
	for _, p := range pois {
		names[p.Name] = p
	}
	if home, ok := names["home"]; !ok || home.X != 1.0 || home.Y != 2.0 || home.Heading != 1.57 {
		t.Errorf("home POI wrong: %+v", names)
	}
}

func TestExtractFromZIP_Errors(t *testing.T) {
	t.Parallel()
	if _, err := ExtractPNGFromZIP([]byte("not a zip")); err == nil {
		t.Errorf("expected error from invalid zip")
	}
	// zip with no relevant entries
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("readme.txt")
	_, _ = w.Write([]byte("hi"))
	_ = zw.Close()
	if _, err := ExtractPNGFromZIP(buf.Bytes()); err == nil {
		t.Errorf("expected 'no PNG' error")
	}
	if _, err := ExtractMetaFromZIP(buf.Bytes()); err == nil {
		t.Errorf("expected 'no YAML' error")
	}
	if _, err := ExtractPOIFromZIP(buf.Bytes()); err == nil {
		t.Errorf("expected 'no path.json' error")
	}
}

func TestParsePOIFromPathJSON(t *testing.T) {
	t.Parallel()
	data := []byte(`{"point":{"a":{"name":"X","x":1,"y":2,"type":"station","angle":0.5},"b":{"name":"y","x":0,"y":0,"type":"wp","angle":0}}}`)
	pois, err := parsePOIFromPathJSON(data)
	if err != nil {
		t.Fatalf("parsePOIFromPathJSON: %v", err)
	}
	if len(pois) != 1 || pois[0].Name != "X" || pois[0].Heading != 0.5 {
		t.Errorf("parsePOIFromPathJSON: %+v", pois)
	}
	if _, err := parsePOIFromPathJSON([]byte("garbage")); err == nil {
		t.Errorf("expected parse error")
	}
}

func TestMapService_ListMapsAndImage(t *testing.T) {
	t.Parallel()
	pngBytes := makePNG(t, 4, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/service/parameter/get/map_list":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"map_list":["mapA","mapB"]}`))
		case "/map_image":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(200)
			_, _ = w.Write(pngBytes)
		case "/map_meta":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"width":4,"height":4,"resolution":0.05,"origin":[0,0,0]}`))
		case "/poi_data":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"point":{"0":{"name":"home","x":1,"y":2,"type":"home","angle":0},"1":{"name":"wp","x":0,"y":0,"type":"wp","angle":0}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	ms := NewMapService(testCfg(srv))
	maps, err := ms.ListMaps()
	if err != nil || len(maps) != 2 || maps[0] != "mapA" {
		t.Fatalf("ListMaps: %v %v", maps, err)
	}
	img, ct, err := ms.GetMapImage("mapA")
	if err != nil || ct != "image/png" || !bytes.Equal(img, pngBytes) {
		t.Fatalf("GetMapImage: ct=%q err=%v eq=%v", ct, err, bytes.Equal(img, pngBytes))
	}
	meta, err := ms.GetMapMeta("mapA")
	if err != nil || meta.Width != 4 || meta.Resolution != 0.05 {
		t.Fatalf("GetMapMeta: %+v %v", meta, err)
	}
	pois, err := ms.GetMapPOI("mapA")
	if err != nil || len(pois) != 1 || pois[0].Name != "home" {
		t.Fatalf("GetMapPOI: %+v %v", pois, err)
	}
}

func TestMapService_ListMaps_AltFormat(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"map list":["only"]}`))
	}))
	defer srv.Close()
	ms := NewMapService(testCfg(srv))
	maps, err := ms.ListMaps()
	if err != nil || len(maps) != 1 || maps[0] != "only" {
		t.Fatalf("ListMaps alt format: %v %v", maps, err)
	}
}

func TestQueryATOMCurrentMap(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"current_map_name":"mapA"}`))
	}))
	defer srv.Close()
	cfg := testCfg(srv)
	if got := queryATOMCurrentMap(cfg); got != "mapA" {
		t.Errorf("queryATOMCurrentMap = %q, want mapA", got)
	}
	state := NewRobotState()
	FetchInitialMapID(NewMapService(cfg), state, cfg)
	if state.Snapshot().MapID != "mapA" {
		t.Errorf("FetchInitialMapID should set MapID")
	}
}
