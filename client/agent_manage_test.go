package client

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateAgentAvatarPresence(t *testing.T) {
	empty, replacement := "", "🐱"
	for _, tc := range []struct {
		name   string
		avatar *string
		want   string
	}{
		{name: "omitted preserves avatar", want: "🤖"},
		{name: "explicit empty clears avatar", avatar: &empty},
		{name: "replacement", avatar: &replacement, want: replacement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/api/v1/agents/agent-1" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				avatar := "🤖"
				if raw, exists := body["avatar"]; exists {
					if tc.avatar == nil {
						t.Errorf("avatar must be omitted, got %s", raw)
					}
					if err := json.Unmarshal(raw, &avatar); err != nil {
						t.Error(err)
					}
				} else if tc.avatar != nil {
					t.Error("explicit avatar must be sent")
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(AgentResponse{
					Success: true, Data: Agent{ID: "agent-1", Avatar: avatar},
				}); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()

			agent, err := NewClient(srv.URL).UpdateAgent(context.Background(), "agent-1", &UpdateAgentRequest{
				Name: "Updated", Avatar: tc.avatar, Config: &AgentConfig{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if agent.Avatar != tc.want {
				t.Errorf("avatar = %q, want %q", agent.Avatar, tc.want)
			}
		})
	}
}

func TestPatchAgentSkills(t *testing.T) {
	selected, all := "selected", "all"
	names, empty := []string{"ppt-generator"}, []string{}
	for _, tc := range []struct {
		name string
		req  PatchAgentRequest
		want string
	}{
		{"omitted", PatchAgentRequest{}, `{}`},
		{"names alone", PatchAgentRequest{SelectedSkills: &names}, `{"selected_skills":["ppt-generator"]}`},
		{
			"selected",
			PatchAgentRequest{SkillsSelectionMode: &selected, SelectedSkills: &names},
			`{"skills_selection_mode":"selected","selected_skills":["ppt-generator"]}`,
		},
		{"empty", PatchAgentRequest{SelectedSkills: &empty}, `{"selected_skills":[]}`},
		{"all", PatchAgentRequest{SkillsSelectionMode: &all}, `{"skills_selection_mode":"all"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/agents/agent-1" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				got, err := json.Marshal(body)
				if err != nil {
					t.Error(err)
				}
				var want map[string]any
				if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
					t.Error(err)
				}
				canonical, err := json.Marshal(want)
				if err != nil || string(got) != string(canonical) {
					t.Errorf("body = %s, want %s (error: %v)", got, canonical, err)
				}
				w.Header().Set("Content-Type", "application/json")
				response := AgentResponse{Success: true, Data: Agent{ID: "agent-1"}}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			agent, err := NewClient(srv.URL).PatchAgent(context.Background(), "agent-1", &tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if agent.ID != "agent-1" {
				t.Errorf("agent ID = %q", agent.ID)
			}
		})
	}
}

func TestAgentConfigRoundTripPreservesKnownAndUnknownFields(t *testing.T) {
	raw := `{
		"agent_mode": "smart-reasoning",
		"model_id": "m-1",
		"thinking": true,
		"reasoning_effort": "low",
		"citation_enabled": false,
		"planning_enabled": true,
		"faq_preflight_model_id": "m-faq",
		"faq_preflight_policy": "strict-policy",
		"future_server_flag": true,
		"future_nested": {"threshold": 0.85, "tags": ["a", "b"]}
	}`
	var cfg AgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.PlanningEnabled || cfg.FAQPreflightModelID != "m-faq" || cfg.FAQPreflightPolicy != "strict-policy" {
		t.Fatalf("missing newly added fields: %+v", cfg)
	}
	if len(cfg.Extra) != 2 {
		t.Fatalf("Extra keys = %v, want 2 unknown keys", cfg.Extra)
	}
	// Mutate a known field and verify round-trip keeps both known & unknown fields.
	cfg.ModelID = "m-2"
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got["model_id"] != "m-2" {
		t.Errorf("model_id = %v, want m-2", got["model_id"])
	}
	if got["faq_preflight_model_id"] != "m-faq" {
		t.Errorf("faq_preflight_model_id = %v, want m-faq", got["faq_preflight_model_id"])
	}
	if got["planning_enabled"] != true {
		t.Errorf("planning_enabled = %v, want true", got["planning_enabled"])
	}
	if got["future_server_flag"] != true {
		t.Errorf("future_server_flag = %v, want true", got["future_server_flag"])
	}
	if nested, ok := got["future_nested"].(map[string]any); !ok || nested["threshold"] != 0.85 {
		t.Errorf("future_nested = %#v, want threshold=0.85", got["future_nested"])
	}
}

func TestAgentConfigCoversServerCustomAgentConfigFields(t *testing.T) {
	serverPath := filepath.Join("..", "internal", "types", "custom_agent.go")
	if _, err := os.Stat(serverPath); err != nil {
		t.Skipf("server source not present at %s: %v", serverPath, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, serverPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", serverPath, err)
	}
	serverKeys := map[string]struct{}{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "CustomAgentConfig" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json")
				if tag == "" || tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if name != "" {
					serverKeys[name] = struct{}{}
				}
			}
		}
	}
	if len(serverKeys) == 0 {
		t.Fatal("failed to extract CustomAgentConfig fields from server source")
	}
	clientKeys := knownAgentConfigJSONKeys()
	var missing []string
	for k := range serverKeys {
		if _, ok := clientKeys[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("client.AgentConfig is missing server CustomAgentConfig JSON fields: %v", missing)
	}
}
