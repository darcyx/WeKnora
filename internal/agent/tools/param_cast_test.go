package tools

import (
	"encoding/json"
	"testing"
)

func TestCastParams_StringToBool(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}}}`)
	args := json.RawMessage(`{"enabled":"true"}`)
	result := CastParams(args, schema)

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["enabled"] != true {
		t.Errorf("expected true, got %v (%T)", parsed["enabled"], parsed["enabled"])
	}
}

func TestCastParams_StringToInt(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}}}`)
	args := json.RawMessage(`{"count":"42"}`)
	result := CastParams(args, schema)

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	// JSON numbers are float64 in Go
	if parsed["count"] != float64(42) {
		t.Errorf("expected 42, got %v (%T)", parsed["count"], parsed["count"])
	}
}

func TestCastParams_StringToFloat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"score":{"type":"number"}}}`)
	args := json.RawMessage(`{"score":"3.14"}`)
	result := CastParams(args, schema)

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["score"] != 3.14 {
		t.Errorf("expected 3.14, got %v", parsed["score"])
	}
}

func TestCastParams_NoChangeNeeded(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	args := json.RawMessage(`{"name":"hello"}`)
	result := CastParams(args, schema)

	if string(result) != string(args) {
		t.Errorf("expected no change, got %s", result)
	}
}

func TestCastParamsPreservesExactNumericConversions(t *testing.T) {
	for _, tc := range []struct{ target, input, want string }{
		{"string", "9007199254740993", `"9007199254740993"`},
		{"string", "9223372036854775807", `"9223372036854775807"`},
		{"string", "1.1234567890123456789", `"1.1234567890123456789"`},
		{"integer", "9007199254740993.0", "9007199254740993"},
		{"integer", "9.007199254740993e15", "9007199254740993"},
		{"integer", "9223372036854775807.0", "9223372036854775807"},
		{"integer", "9223372036854775808.0", "9223372036854775808.0"},
		{"integer", "9007199254740993.1", "9007199254740993.1"},
		{"boolean", "1.00000000000000000001", "1.00000000000000000001"},
		{"boolean", "1e0", "true"},
		{"boolean", "1e-10000", "1e-10000"},
	} {
		t.Run(tc.target+"/"+tc.input, func(t *testing.T) {
			schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"` + tc.target + `"}}}`)
			result := CastParams(json.RawMessage(`{"value":`+tc.input+`}`), schema)
			var values map[string]json.RawMessage
			if err := json.Unmarshal(result, &values); err != nil {
				t.Fatal(err)
			}
			if string(values["value"]) != tc.want {
				t.Fatalf("got %s, want exact literal %s", values["value"], tc.want)
			}
		})
	}
}

func TestCastParams_NilSchema(t *testing.T) {
	args := json.RawMessage(`{"foo":"bar"}`)
	result := CastParams(args, nil)
	if string(result) != string(args) {
		t.Errorf("expected no change with nil schema")
	}
}

func TestCastParams_BoolFalseString(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"flag":{"type":"boolean"}}}`)
	args := json.RawMessage(`{"flag":"false"}`)
	result := CastParams(args, schema)

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["flag"] != false {
		t.Errorf("expected false, got %v (%T)", parsed["flag"], parsed["flag"])
	}
}

func TestCastParamsOnlyUnwrapsUnambiguousNullableArrays(t *testing.T) {
	for _, tc := range []struct{ types, input, want string }{
		{`["null","array"]`, `"[\"a\"]"`, `["a"]`},
		{`["array","null"]`, `" [42]"`, `[42]`},
		{`["array","string"]`, `"[42]"`, `"[42]"`},
		{`["array","null"]`, `null`, `null`},
		{`["array","null"]`, `"[broken"`, `"[broken"`},
		{`["array","null"]`, `"a"`, `"a"`},
	} {
		schema := json.RawMessage(`{"properties":{"ids":{"type":` + tc.types + `}}}`)
		args := json.RawMessage(`{"ids":` + tc.input + `}`)
		if got := string(CastParams(args, schema)); got != `{"ids":`+tc.want+`}` {
			t.Errorf("types=%s input=%s: got %s, want ids=%s", tc.types, tc.input, got, tc.want)
		}
	}
}

func TestCastParams_StringToStringArray(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"patterns":{"type":"array","items":{"type":"string"}}}}`)
	args := json.RawMessage(`{"patterns":"OpenClaw"}`)
	result := CastParams(args, schema)

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}

	patterns, ok := parsed["patterns"].([]interface{})
	if !ok {
		t.Fatalf("expected patterns to be array, got %T", parsed["patterns"])
	}
	if len(patterns) != 1 || patterns[0] != "OpenClaw" {
		t.Fatalf("expected [OpenClaw], got %v", patterns)
	}
}
