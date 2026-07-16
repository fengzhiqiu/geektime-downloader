package config

import (
	"testing"
	"time"
)

func validServeCfg() AppConfig {
	return AppConfig{
		Quality: "sd", DownloadComments: 1, ColumnOutputType: 7,
		LogLevel: "info", Interval: 1,
		JobTimeout: 60 * time.Minute, HeartbeatTimeout: 10 * time.Minute,
		RateLimitCooldown: 120 * time.Second, SegmentTimeout: 60 * time.Second,
	}
}

func TestValidateServeConfigStabilityZeroRejected(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AppConfig)
	}{
		{"JobTimeout zero", func(c *AppConfig) { c.JobTimeout = 0 }},
		{"HeartbeatTimeout zero", func(c *AppConfig) { c.HeartbeatTimeout = 0 }},
		{"RateLimitCooldown zero", func(c *AppConfig) { c.RateLimitCooldown = 0 }},
		{"SegmentTimeout zero", func(c *AppConfig) { c.SegmentTimeout = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validServeCfg()
			tc.mut(&c)
			if err := ValidateServeConfig(&c); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
