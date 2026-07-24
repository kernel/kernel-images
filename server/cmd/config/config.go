package config

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kelseyhightower/envconfig"

	"github.com/kernel/kernel-images/server/lib/events"
)

// Config holds all configuration for the server
type Config struct {
	// Server configuration
	Port int `envconfig:"PORT" default:"10001"`

	// Port for the Prometheus metrics endpoint. Served on a separate
	// listener so scrapes bypass the scale-to-zero middleware and the
	// external API surface.
	MetricsPort int `envconfig:"METRICS_PORT" default:"10002"`

	// Recording configuration
	FrameRate   int    `envconfig:"FRAME_RATE" default:"10"`
	DisplayNum  int    `envconfig:"DISPLAY_NUM" default:"1"`
	MaxSizeInMB int    `envconfig:"MAX_SIZE_MB" default:"500"`
	OutputDir   string `envconfig:"OUTPUT_DIR" default:"."`
	// AudioSource and PulseServer default to empty, i.e. video-only. Setting both
	// enables audio capture; their values must match the topology defined in
	// shared/start-pulseaudio.sh (the authority for the sink/source/socket). The
	// image's supervisor conf sets both.
	AudioSource string `envconfig:"AUDIO_SOURCE" default:""`
	PulseServer string `envconfig:"PULSE_SERVER" default:""`

	// Absolute or relative path to the ffmpeg binary. If empty the code falls back to "ffmpeg" on $PATH.
	PathToFFmpeg string `envconfig:"FFMPEG_PATH" default:"ffmpeg"`

	// DevTools proxy configuration
	DevToolsProxyPort int  `envconfig:"DEVTOOLS_PROXY_PORT" default:"9222"`
	LogCDPMessages    bool `envconfig:"LOG_CDP_MESSAGES" default:"false"`

	// How long to wait after the last active request before re-enabling scale-to-zero.
	ScaleToZeroCooldown time.Duration `envconfig:"SCALE_TO_ZERO_COOLDOWN" default:"1s"`

	// ChromeDriver proxy: external port where the proxy listens.
	ChromeDriverProxyPort int `envconfig:"CHROMEDRIVER_PROXY_PORT" default:"9224"`
	// Internal ChromeDriver upstream used by the ChromeDriver proxy.
	ChromeDriverUpstreamAddr string `envconfig:"CHROMEDRIVER_UPSTREAM_ADDR" default:"127.0.0.1:9225"`
	// DevTools proxy address passed to ChromeDriver as goog:chromeOptions.debuggerAddress.
	// If empty, it is derived from DevToolsProxyPort as 127.0.0.1:<port>.
	DevToolsProxyAddr string `envconfig:"DEVTOOLS_PROXY_ADDR" default:""`

	// S2 durable event storage. All three fields must be set to enable the S2 sink.
	S2Basin       string `envconfig:"S2_BASIN"        default:""`
	S2AccessToken string `envconfig:"S2_ACCESS_TOKEN" default:""`
	S2Stream      string `envconfig:"S2_STREAM"       default:""`

	// Browser-telemetry OTLP export. OTLPEndpoint (host[:port]) must be set to
	// enable the OTLP sink. The BTEL_ prefix avoids collision with the standard
	// OTEL_ vars that configure the API server's own telemetry.
	OTLPEndpoint    string `envconfig:"BTEL_OTLP_ENDPOINT"     default:""`
	OTLPPath        string `envconfig:"BTEL_OTLP_PATH"         default:"/v1/logs"`
	OTLPInsecure    bool   `envconfig:"BTEL_OTLP_INSECURE"     default:"false"`
	OTLPServiceName string `envconfig:"BTEL_OTLP_SERVICE_NAME" default:"kernel-browser"`
	// OTLP batch tuning. Defaults match the OTel SDK's, so leaving these unset
	// preserves prior behavior. MaxQueueSize is the backpressure buffer: once
	// full, the oldest records are dropped (see the drop metric). It must be at
	// least the export batch size (validated); a smaller queue would drop before
	// a batch fills.
	OTLPMaxQueueSize   int           `envconfig:"BTEL_OTLP_MAX_QUEUE_SIZE" default:"2048"`
	OTLPExportInterval time.Duration `envconfig:"BTEL_OTLP_EXPORT_INTERVAL" default:"1s"`
	OTLPExportTimeout  time.Duration `envconfig:"BTEL_OTLP_EXPORT_TIMEOUT"  default:"30s"`
	// Platform-injected identity, reused to stamp the OTLP Resource. These are
	// the same envs the VM already receives.
	InstanceJWT  string `envconfig:"KERNEL_INSTANCE_JWT" default:""`
	InstanceName string `envconfig:"INST_NAME"           default:""`
	MetroName    string `envconfig:"METRO_NAME"          default:""`
}

// LogValue implements slog.LogValuer, redacting secret fields.
func (c *Config) LogValue() slog.Value {
	s2AccessToken := ""
	if c.S2AccessToken != "" {
		s2AccessToken = "[redacted]"
	}
	otlpJWT := ""
	if c.InstanceJWT != "" {
		otlpJWT = "[redacted]"
	}
	return slog.GroupValue(
		slog.Int("port", c.Port),
		slog.Int("metrics_port", c.MetricsPort),
		slog.Int("frame_rate", c.FrameRate),
		slog.Int("display_num", c.DisplayNum),
		slog.Int("max_size_mb", c.MaxSizeInMB),
		slog.String("output_dir", c.OutputDir),
		slog.String("audio_source", c.AudioSource),
		slog.String("pulse_server", c.PulseServer),
		slog.String("ffmpeg_path", c.PathToFFmpeg),
		slog.Int("devtools_proxy_port", c.DevToolsProxyPort),
		slog.Bool("log_cdp_messages", c.LogCDPMessages),
		slog.Duration("scale_to_zero_cooldown", c.ScaleToZeroCooldown),
		slog.Int("chromedriver_proxy_port", c.ChromeDriverProxyPort),
		slog.String("chromedriver_upstream_addr", c.ChromeDriverUpstreamAddr),
		slog.String("devtools_proxy_addr", c.DevToolsProxyAddr),
		slog.String("s2_basin", c.S2Basin),
		slog.String("s2_access_token", s2AccessToken),
		slog.String("s2_stream", c.S2Stream),
		slog.String("otlp_endpoint", c.OTLPEndpoint),
		slog.String("otlp_path", c.OTLPPath),
		slog.Bool("otlp_insecure", c.OTLPInsecure),
		slog.String("otlp_instance_jwt", otlpJWT),
		slog.String("otlp_service_name", c.OTLPServiceName),
		slog.Int("otlp_max_queue_size", c.OTLPMaxQueueSize),
		slog.Duration("otlp_export_interval", c.OTLPExportInterval),
		slog.Duration("otlp_export_timeout", c.OTLPExportTimeout),
		slog.String("otlp_instance_name", c.InstanceName),
		slog.String("otlp_metro", c.MetroName),
	)
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	var config Config
	if err := envconfig.Process("", &config); err != nil {
		return nil, err
	}
	if config.DevToolsProxyAddr == "" {
		config.DevToolsProxyAddr = fmt.Sprintf("127.0.0.1:%d", config.DevToolsProxyPort)
	}
	if err := validate(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

func validate(config *Config) error {
	if config.OutputDir == "" {
		return fmt.Errorf("OUTPUT_DIR is required")
	}
	if config.DisplayNum < 0 {
		return fmt.Errorf("DISPLAY_NUM must be greater than 0")
	}
	if config.FrameRate < 0 || config.FrameRate > 20 {
		return fmt.Errorf("FRAME_RATE must be greater than 0 and less than or equal to 20")
	}
	if config.MaxSizeInMB < 0 || config.MaxSizeInMB > 1000 {
		return fmt.Errorf("MAX_SIZE_MB must be greater than 0 and less than or equal to 1000")
	}
	if config.PathToFFmpeg == "" {
		return fmt.Errorf("FFMPEG_PATH is required")
	}
	if config.ChromeDriverUpstreamAddr == "" {
		return fmt.Errorf("CHROMEDRIVER_UPSTREAM_ADDR is required")
	}
	if config.DevToolsProxyAddr == "" {
		return fmt.Errorf("DEVTOOLS_PROXY_ADDR is required")
	}
	// The queue must hold at least one full export batch; a smaller value would
	// drop records before a batch ever fills. Validated against the batch-size
	// constant so the two can't drift.
	if config.OTLPMaxQueueSize < events.DefaultOTLPMaxBatchRecords {
		return fmt.Errorf("BTEL_OTLP_MAX_QUEUE_SIZE must be at least %d (the export batch size)", events.DefaultOTLPMaxBatchRecords)
	}
	if config.OTLPExportInterval <= 0 {
		return fmt.Errorf("BTEL_OTLP_EXPORT_INTERVAL must be greater than 0")
	}
	if config.OTLPExportTimeout <= 0 {
		return fmt.Errorf("BTEL_OTLP_EXPORT_TIMEOUT must be greater than 0")
	}

	return nil
}
