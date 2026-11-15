package main

import "testing"

func TestConfigSafety(t *testing.T) {
	c, err := parseConfig([]byte("dataDir: /safe\n"))
	if err != nil || c.SchedulerMode != "shadow" || c.MaxAgeSeconds != 600 || c.PollIntervalSeconds != 180 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, s := range []string{"dataDir: /safe\nmanagementURL: https://example.com\n", "dataDir: /safe\nschedulerMode: banana\n", "schedulerMode: shadow\n", "dataDir: /safe\nproviderStrategies: {codex: unexpected}\n", "dataDir: /safe\nmanagementURL: http://localhost:8317\n"} {
		if _, err := parseConfig([]byte(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	if _, err := parseConfig([]byte("dataDir: /safe\nmanagementURL: http://127.0.0.1:8317\nmanagementKeyEnv: CPA_MGMT\n")); err != nil {
		t.Fatal(err)
	}
}
