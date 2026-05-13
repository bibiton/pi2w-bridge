package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/joho/godotenv"
)

// ServerConfig holds process-wide settings shared by all robot sessions.
type ServerConfig struct {
	ListenAddr    string // webhook + admin HTTP server, e.g. ":5201"
	PublicBaseURL string // how robots reach our webhook, e.g. "https://bridge.example.com"
	DBPath        string // SQLite path or full DSN

	MQTTBroker string
	MQTTUser   string
	MQTTPass   string
	MQTTPrefix string

	Manufacturer string // default VDA manufacturer when a RobotRecord omits it
	RobotPort    string // default port for the robot's HTTP/WS API when a URL omits one
	TTSURL       string // default atomros2-tts URL; empty disables voice

	AdminToken         string // bearer token for /admin/*
	DefaultRobotSecret string // X-Webhook-Secret used when a RobotRecord omits one
}

func LoadServerConfig() *ServerConfig {
	_ = godotenv.Load()
	return &ServerConfig{
		ListenAddr:         envOrDefault("LISTEN_ADDR", ":5201"),
		PublicBaseURL:      envOrDefault("PUBLIC_BASE_URL", "http://127.0.0.1:5201"),
		DBPath:             envOrDefault("DB_PATH", "pi2w-bridge.db"),
		MQTTBroker:         envOrDefault("MQTT_BROKER", "wss://nexmqtt.jini.tw:443/mqtt"),
		MQTTUser:           envOrDefault("MQTT_USER", "bibi"),
		MQTTPass:           envOrDefault("MQTT_PASS", "70595145"),
		MQTTPrefix:         envOrDefault("MQTT_PREFIX", "/uagv/v2"),
		Manufacturer:       envOrDefault("VDA_MANUFACTURER", "Atom"),
		RobotPort:          envOrDefault("ROBOT_PORT", "8080"),
		TTSURL:             envOrDefault("TTS_URL", ""),
		AdminToken:         envOrDefault("ADMIN_TOKEN", "pi2w-admin-changeme"),
		DefaultRobotSecret: envOrDefault("DEFAULT_ROBOT_SECRET", "pi2w-webhook-changeme"),
	}
}

// RobotRecord is the persisted/declared description of one robot.
type RobotRecord struct {
	ID             string `json:"id" yaml:"id"`
	Manufacturer   string `json:"manufacturer" yaml:"manufacturer"`
	Serial         string `json:"serial" yaml:"serial"`
	AtomBaseURL    string `json:"atomBaseURL" yaml:"atomBaseURL"`       // http://ip:8080
	FastAPIHTTPURL string `json:"fastapiHTTPURL" yaml:"fastapiHTTPURL"` // http://ip:8000
	FastAPIWSURL   string `json:"fastapiWSURL" yaml:"fastapiWSURL"`     // ws://ip:8000/ws
	WebhookSecret  string `json:"webhookSecret" yaml:"webhookSecret"`
	Status         string `json:"status" yaml:"-"` // online|offline|errored|provisional|deleted
	Source         string `json:"source" yaml:"-"` // db|yaml|provisional
	LastSeenAt     int64  `json:"lastSeenAt" yaml:"-"`
}

// hostPort extracts host and port from a URL like http://1.2.3.4:8080.
func hostPort(rawURL, defPort string) (host, port string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL, defPort
	}
	h := u.Hostname()
	p := u.Port()
	if p == "" {
		p = defPort
	}
	return h, p
}

// LoadConfigForRobot builds the per-session *Config from a RobotRecord + server defaults.
func LoadConfigForRobot(rec RobotRecord, srv *ServerConfig) *Config {
	mfr := rec.Manufacturer
	if mfr == "" {
		mfr = srv.Manufacturer
	}
	serial := rec.Serial
	if serial == "" {
		serial = rec.ID
	}
	defPort := srv.RobotPort
	if defPort == "" {
		defPort = "8080"
	}
	ip, port := hostPort(rec.AtomBaseURL, defPort)
	// FastAPI defaults to the same host:port as the ATOM API (many deployments
	// serve both on one port). Override per robot via fastapiHTTPURL / fastapiWSURL.
	fastapi := rec.FastAPIHTTPURL
	if fastapi == "" && ip != "" {
		fastapi = "http://" + net.JoinHostPort(ip, port)
	}
	secret := rec.WebhookSecret
	if secret == "" {
		secret = srv.DefaultRobotSecret
	}
	return &Config{
		RobotIP:        ip,
		RobotPort:      port,
		RobotFastAPI:   strings.TrimRight(fastapi, "/"),
		RobotFastAPIWS: rec.FastAPIWSURL,
		WebhookSecret:  secret,
		MQTTBroker:     srv.MQTTBroker,
		MQTTUser:       srv.MQTTUser,
		MQTTPass:       srv.MQTTPass,
		MQTTPrefix:     srv.MQTTPrefix,
		Manufacturer:   mfr,
		SerialNumber:   serial,
		TTSURL:         srv.TTSURL,
	}
}
