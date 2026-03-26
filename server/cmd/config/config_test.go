package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	testCases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		wantCfg *Config
	}{
		{
			name: "defaults (no env set)",
			env:  map[string]string{},
			wantCfg: &Config{
				Port:                     10001,
				FrameRate:                10,
				DisplayNum:               1,
				MaxSizeInMB:              500,
				OutputDir:                ".",
				PathToFFmpeg:             "ffmpeg",
				DevToolsProxyPort:        9222,
				ChromeDriverProxyPort:    9224,
				ChromeDriverUpstreamAddr: "127.0.0.1:9225",
				DevToolsProxyAddr:        "127.0.0.1:9222",
			},
		},
		{
			name: "custom valid env",
			env: map[string]string{
				"PORT":                       "12345",
				"FRAME_RATE":                 "20",
				"DISPLAY_NUM":                "2",
				"MAX_SIZE_MB":                "250",
				"OUTPUT_DIR":                 "/tmp",
				"FFMPEG_PATH":                "/usr/local/bin/ffmpeg",
				"DEVTOOLS_PROXY_PORT":        "9876",
				"CHROMEDRIVER_PROXY_PORT":    "5432",
				"CHROMEDRIVER_UPSTREAM_ADDR": "127.0.0.1:9999",
			},
			wantCfg: &Config{
				Port:                     12345,
				FrameRate:                20,
				DisplayNum:               2,
				MaxSizeInMB:              250,
				OutputDir:                "/tmp",
				PathToFFmpeg:             "/usr/local/bin/ffmpeg",
				DevToolsProxyPort:        9876,
				ChromeDriverProxyPort:    5432,
				ChromeDriverUpstreamAddr: "127.0.0.1:9999",
				DevToolsProxyAddr:        "127.0.0.1:9876",
			},
		},
		{
			name: "explicit devtools proxy addr override",
			env: map[string]string{
				"DEVTOOLS_PROXY_PORT": "7777",
				"DEVTOOLS_PROXY_ADDR": "10.0.0.1:1234",
			},
			wantCfg: &Config{
				Port:                     10001,
				FrameRate:                10,
				DisplayNum:               1,
				MaxSizeInMB:              500,
				OutputDir:                ".",
				PathToFFmpeg:             "ffmpeg",
				DevToolsProxyPort:        7777,
				ChromeDriverProxyPort:    9224,
				ChromeDriverUpstreamAddr: "127.0.0.1:9225",
				DevToolsProxyAddr:        "10.0.0.1:1234",
			},
		},
		{
			name: "negative display num",
			env: map[string]string{
				"DISPLAY_NUM": "-1",
			},
			wantErr: true,
		},
		{
			name: "frame rate too high",
			env: map[string]string{
				"FRAME_RATE": "1201",
			},
			wantErr: true,
		},
		{
			name: "max size too big",
			env: map[string]string{
				"MAX_SIZE_MB": "10001",
			},
			wantErr: true,
		},
		{
			name: "missing ffmpeg path (set to empty)",
			env: map[string]string{
				"FFMPEG_PATH": "",
			},
			wantErr: true,
		},
		{
			name: "missing output dir (set to empty)",
			env: map[string]string{
				"OUTPUT_DIR": "",
			},
			wantErr: true,
		},
		{
			name: "missing chromedriver upstream addr (set to empty)",
			env: map[string]string{
				"CHROMEDRIVER_UPSTREAM_ADDR": "",
			},
			wantErr: true,
		},
	}

	for idx := range testCases {
		tc := testCases[idx]
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, cfg)
				require.Equal(t, tc.wantCfg, cfg)
			}
		})
	}
}
