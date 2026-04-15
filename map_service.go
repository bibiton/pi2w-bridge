package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type MapService struct {
	fastAPIURL string // robot FastAPI URL (port 8000)
	atomAPIURL string // robot ATOM API URL (port 8080)
	client     *http.Client
}

type MapMeta struct {
	Resolution float64   `json:"resolution"`
	Origin     []float64 `json:"origin"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
}

type MapPOI struct {
	Name    string  `json:"name"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Heading float64 `json:"angle"`
	Type    string  `json:"type,omitempty"`
}

func NewMapService(cfg *Config) *MapService {
	return &MapService{
		fastAPIURL: cfg.RobotFastAPI,
		atomAPIURL: cfg.RobotBaseURL(),
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// ListMaps returns the list of available map names from ATOM API.
func (ms *MapService) ListMaps() ([]string, error) {
	url := ms.atomAPIURL + "/service/parameter/get/map_list"
	resp, err := ms.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("ATOM map_list: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Try {"map_list": [...]}
	var result struct {
		MapList []string `json:"map_list"`
	}
	if err := json.Unmarshal(body, &result); err == nil && len(result.MapList) > 0 {
		return result.MapList, nil
	}

	// Try {"map list": [...]}
	var result2 map[string]json.RawMessage
	if err := json.Unmarshal(body, &result2); err == nil {
		if raw, ok := result2["map list"]; ok {
			var maps []string
			if err := json.Unmarshal(raw, &maps); err == nil {
				return maps, nil
			}
		}
	}

	return nil, fmt.Errorf("cannot parse map_list response: %s", string(body))
}

// GetMapImage returns the map image bytes (PNG) from FastAPI port 8000.
func (ms *MapService) GetMapImage(mapName string) ([]byte, string, error) {
	url := ms.fastAPIURL + "/map_image"
	resp, err := ms.client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("FastAPI map_image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("FastAPI map_image HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read image: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/png"
	}
	return body, ct, nil
}

// GetMapMeta returns the map metadata from FastAPI port 8000.
func (ms *MapService) GetMapMeta(mapName string) (*MapMeta, error) {
	url := ms.fastAPIURL + "/map_meta"
	resp, err := ms.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("FastAPI map_meta: %w", err)
	}
	defer resp.Body.Close()

	var raw struct {
		Width      int       `json:"width"`
		Height     int       `json:"height"`
		Resolution float64   `json:"resolution"`
		Origin     []float64 `json:"origin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode FastAPI meta: %w", err)
	}

	return &MapMeta{
		Resolution: raw.Resolution,
		Origin:     raw.Origin,
		Width:      raw.Width,
		Height:     raw.Height,
	}, nil
}

// GetMapPOI returns the list of POI/waypoints from FastAPI port 8000 /poi_data.
func (ms *MapService) GetMapPOI(mapName string) ([]MapPOI, error) {
	url := ms.fastAPIURL + "/poi_data"
	resp, err := ms.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("FastAPI poi_data: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read poi_data: %w", err)
	}

	// FastAPI returns: {"point":{"0":{"x":0,"y":0,"name":"home","type":"home","angle":0},...}, "path":{...}, "wall":{...}}
	var raw struct {
		Point map[string]struct {
			Name  string  `json:"name"`
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
			Type  string  `json:"type"`
			Angle float64 `json:"angle"`
		} `json:"point"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse FastAPI poi_data: %w", err)
	}

	var pois []MapPOI
	for _, p := range raw.Point {
		// Skip navigation waypoints, only keep actual destinations
		if p.Type == "wp" {
			continue
		}
		pois = append(pois, MapPOI{
			Name:    p.Name,
			X:       p.X,
			Y:       p.Y,
			Heading: p.Angle,
			Type:    p.Type,
		})
	}

	log.Printf("[MapService] Got %d POIs from FastAPI /poi_data", len(pois))
	return pois, nil
}

// DownloadMapZIP downloads a map ZIP from ATOM API and returns it in memory.
// The ZIP is NOT saved to disk to conserve Pi storage.
func (ms *MapService) DownloadMapZIP(mapName string) ([]byte, error) {
	url := ms.atomAPIURL + "/service/parameter/get/map/" + mapName
	client := &http.Client{Timeout: 60 * time.Second} // large ZIPs need more time
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("ATOM get map ZIP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("map %s not found on robot", mapName)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ATOM get map ZIP HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ZIP body: %w", err)
	}

	log.Printf("[MapService] Downloaded ZIP for map %s (%d bytes)", mapName, len(data))
	return data, nil
}

// ExtractPNGFromZIP extracts the map PNG (WHEELTEC.png) from a map ZIP in memory.
func ExtractPNGFromZIP(zipData []byte) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("open ZIP: %w", err)
	}

	for _, f := range r.File {
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, ".png") && !strings.Contains(name, "review") {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open %s in ZIP: %w", f.Name, err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", f.Name, err)
			}
			log.Printf("[MapService] Extracted %s from ZIP (%d bytes)", f.Name, len(data))
			return data, nil
		}
	}
	return nil, fmt.Errorf("no PNG found in ZIP")
}

// ExtractMetaFromZIP extracts map metadata from WHEELTEC.yaml in the ZIP.
func ExtractMetaFromZIP(zipData []byte) (*MapMeta, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("open ZIP: %w", err)
	}

	for _, f := range r.File {
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, ".yaml") && !strings.Contains(name, "keepout") {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open %s in ZIP: %w", f.Name, err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", f.Name, err)
			}

			var yamlData struct {
				Resolution float64   `yaml:"resolution"`
				Origin     []float64 `yaml:"origin"`
			}
			if err := yaml.Unmarshal(data, &yamlData); err != nil {
				return nil, fmt.Errorf("parse YAML: %w", err)
			}

			// Get image dimensions from the PNG in the same ZIP
			pngData, err := ExtractPNGFromZIP(zipData)
			if err != nil {
				return nil, fmt.Errorf("extract PNG for dimensions: %w", err)
			}

			width, height := getImageDimensions(pngData)

			origin := yamlData.Origin
			if len(origin) < 2 {
				origin = []float64{0, 0}
			}

			return &MapMeta{
				Resolution: yamlData.Resolution,
				Origin:     origin[:2],
				Width:      width,
				Height:     height,
			}, nil
		}
	}
	return nil, fmt.Errorf("no YAML found in ZIP")
}

// ExtractPOIFromZIP extracts POI/waypoints from path.json in the ZIP.
func ExtractPOIFromZIP(zipData []byte) ([]MapPOI, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("open ZIP: %w", err)
	}

	for _, f := range r.File {
		if strings.ToLower(f.Name) == "path.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open path.json: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, fmt.Errorf("read path.json: %w", err)
			}

			return parsePOIFromPathJSON(data)
		}
	}
	return nil, fmt.Errorf("no path.json found in ZIP")
}

// parsePOIFromPathJSON parses the path.json format into MapPOI slice.
func parsePOIFromPathJSON(data []byte) ([]MapPOI, error) {
	var raw struct {
		Point map[string]struct {
			Name  string  `json:"name"`
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
			Type  string  `json:"type"`
			Angle float64 `json:"angle"`
		} `json:"point"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse path.json: %w", err)
	}

	var pois []MapPOI
	for _, p := range raw.Point {
		if p.Type == "wp" {
			continue
		}
		pois = append(pois, MapPOI{
			Name:    p.Name,
			X:       p.X,
			Y:       p.Y,
			Heading: p.Angle,
			Type:    p.Type,
		})
	}
	return pois, nil
}

// getImageDimensions reads PNG header to get width/height without full decode.
func getImageDimensions(pngData []byte) (int, int) {
	// PNG header: 8 bytes signature, then IHDR chunk
	// IHDR starts at offset 8: 4 bytes length, 4 bytes "IHDR", 4 bytes width, 4 bytes height
	if len(pngData) < 24 {
		return 0, 0
	}
	w := int(pngData[16])<<24 | int(pngData[17])<<16 | int(pngData[18])<<8 | int(pngData[19])
	h := int(pngData[20])<<24 | int(pngData[21])<<16 | int(pngData[22])<<8 | int(pngData[23])
	return w, h
}

// FetchInitialMapID gets the current map from ATOM API and updates state.
func FetchInitialMapID(ms *MapService, state *RobotState, cfg *Config) {
	mapName := queryATOMCurrentMap(cfg)
	if mapName != "" {
		log.Printf("[MapService] Current map: %s", mapName)
		state.SetMapID(mapName)
	} else {
		log.Printf("[MapService] WARNING: Could not determine current map")
	}
}

// queryATOMCurrentMap queries the ATOM API for current_map_name.
func queryATOMCurrentMap(cfg *Config) string {
	url := cfg.RobotBaseURL() + "/service/parameter/get/current_map_name"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("[MapService] ATOM API current_map_name error: %v", err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return ""
	}

	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	name := result["current_map_name"]
	if name == "" {
		name = result["current map name"]
	}
	return name
}

// StartMapListLoop periodically refreshes the map list and updates state.
func StartMapListLoop(ms *MapService, state *RobotState) {
	go func() {
		updateMapList(ms, state)

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			updateMapList(ms, state)
		}
	}()
}

func updateMapList(ms *MapService, state *RobotState) {
	maps, err := ms.ListMaps()
	if err != nil {
		log.Printf("[MapService] Failed to list maps: %v", err)
		return
	}
	log.Printf("[MapService] Map list updated: %d maps", len(maps))
	state.SetMapList(maps)
}
