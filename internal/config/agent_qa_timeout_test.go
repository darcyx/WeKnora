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

func TestAgentFAQPreflightModelEnvOverride(t *testing.T) {
	t.Setenv("WEKNORA_AGENT_FAQ_PREFLIGHT_MODEL_ID", "  fast-faq-model-id  ")
	t.Setenv("WEKNORA_AGENT_FAQ_PREFLIGHT_TIMEOUT", "4s")
	cfg := &Config{}
	if err := applyAgentEnvOverrides(cfg); err != nil {
		t.Fatalf("applyAgentEnvOverrides: %v", err)
	}
	if cfg.Agent.FAQPreflightModelID != "fast-faq-model-id" {
		t.Fatalf("FAQPreflightModelID=%q", cfg.Agent.FAQPreflightModelID)
	}
	if cfg.Agent.FAQPreflightTimeoutSeconds != 4 {
		t.Fatalf("FAQPreflightTimeoutSeconds=%d", cfg.Agent.FAQPreflightTimeoutSeconds)
	}
	t.Setenv("WEKNORA_AGENT_FAQ_PREFLIGHT_TIMEOUT", "500ms")
	cfgSub := &Config{}
	if err := applyAgentEnvOverrides(cfgSub); err != nil {
		t.Fatalf("applyAgentEnvOverrides sub-second: %v", err)
	}
	if cfgSub.Agent.FAQPreflightTimeoutSeconds != 0 {
		t.Fatalf("sub-second FAQPreflightTimeoutSeconds=%d", cfgSub.Agent.FAQPreflightTimeoutSeconds)
	}
}
