package config

import "testing"

func TestAgentQATotalTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		seconds int
		valid   bool
	}{
		{"0", 0, true}, {"12", 12, true}, {"2m", 120, true}, {"-1", 0, false}, {"bad", 0, false}, {"1ms", 0, false}, {"999999999999999999999999", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("WEKNORA_AGENT_QA_TOTAL_TIMEOUT", tc.value)
			cfg := &Config{}
			err := applyAgentEnvOverrides(cfg)
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
			if tc.valid && cfg.Agent.QATotalTimeoutSeconds != tc.seconds {
				t.Fatalf("seconds=%d", cfg.Agent.QATotalTimeoutSeconds)
			}
		})
	}
	for _, seconds := range []int{-1, int((1<<63-1)/1000000000) + 1} {
		if err := ValidateConfig(&Config{Agent: &AgentConfig{QATotalTimeoutSeconds: seconds}}); err == nil {
			t.Fatalf("accepted %d", seconds)
		}
	}
}
