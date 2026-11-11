package management

import (
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// pathPatchFixture is written in the encoder's own layout, so a targeted
// mutation must leave every untouched line byte-identical.
const pathPatchFixture = `# top comment
port: 8317
api-keys:
  - key-one
  - key-two
openai-compatibility:
  - name: alpha
    base-url: https://alpha.example
    models:
      - name: shared-model
        alias: alpha-shared
  - name: beta
    base-url: https://beta.example
    models:
      - name: shared-model
        alias: beta-shared
reasoning-policy:
  codex:
    default: high # line comment
    models:
      gpt-luna: medium
custom-section:
  null-value: null
  empty-map: {}
  empty-list: []
  quoted-bool: "true"
  quoted-number: "0123"
  float: 1.5
  flag: false
`

func patchFixture(t *testing.T, initial, body string) (*httptest_response, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, path := configYAMLTestHandler(t, initial)
	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d", get.Code)
	}
	response := performConfigYAMLPathPatch(h, body, get.Header().Get("ETag"))
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &httptest_response{code: response.Code, body: response.Body.String()}, string(stored)
}

type httptest_response struct {
	code int
	body string
}

func TestPatchConfigYAMLPathKeepsUntouchedBytes(t *testing.T) {
	response, stored := patchFixture(t, pathPatchFixture, `{"operations":[{"path":["reasoning-policy","codex","default"],"value":"low","replace":true}]}`)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	want := strings.Replace(pathPatchFixture, "default: high # line comment", "default: low # line comment", 1)
	if stored != want {
		t.Fatalf("stored:\n%s\nwant:\n%s", stored, want)
	}
}

func TestPatchConfigYAMLPathAddsAndDeletesMappingKeys(t *testing.T) {
	body := `{"operations":[` +
		`{"path":["reasoning-policy","codex","models","gpt-luna"],"delete":true},` +
		`{"path":["reasoning-policy","claude","default"],"value":"medium","replace":true},` +
		`{"path":["custom-section","added"],"value":{"nested":[1,"two",null]},"replace":true}]}`
	response, stored := patchFixture(t, pathPatchFixture, body)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	want := strings.Replace(pathPatchFixture, "    models:\n      gpt-luna: medium\n", "    models: {}\n", 1)
	want = strings.Replace(want, "custom-section:", "  claude:\n    default: medium\ncustom-section:", 1)
	want += "  added:\n    nested:\n      - 1\n      - two\n      - null\n"
	if stored != want {
		t.Fatalf("stored:\n%s\nwant:\n%s", stored, want)
	}
}

func TestPatchConfigYAMLPathReplacesOnlyTheTargetedList(t *testing.T) {
	// Removing provider "alpha" must not touch "beta", even though both list a
	// model with the same name.
	body := `{"operations":[{"path":["openai-compatibility"],"value":[{"name":"beta","base-url":"https://beta.example","models":[{"name":"shared-model","alias":"beta-shared"}]}],"replace":true}]}`
	response, stored := patchFixture(t, pathPatchFixture, body)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	alpha := "  - name: alpha\n    base-url: https://alpha.example\n    models:\n      - name: shared-model\n        alias: alpha-shared\n"
	if want := strings.Replace(pathPatchFixture, alpha, "", 1); stored != want {
		t.Fatalf("stored:\n%s\nwant:\n%s", stored, want)
	}
}

func TestPatchConfigYAMLPathWritesMultilineInstructionAsValidYAML(t *testing.T) {
	instruction := "FROM codex:gpt\r\n\r\n# Deployment Configuration\r\n  indented\r\npreserved text"
	body := `{"operations":[{"path":["codex","system-instruction"],"value":"FROM codex:gpt\r\n\r\n# Deployment Configuration\r\n  indented\r\npreserved text","replace":true}]}`
	response, stored := patchFixture(t, "port: 8317\ncodex:\n  system-instruction: old\n", body)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	var parsed struct {
		Codex map[string]string `yaml:"codex"`
	}
	if err := yaml.Unmarshal([]byte(stored), &parsed); err != nil {
		t.Fatalf("stored config does not parse: %v\n%s", err, stored)
	}
	if parsed.Codex["system-instruction"] != instruction {
		t.Fatalf("instruction = %q", parsed.Codex["system-instruction"])
	}
	for _, line := range strings.Split(stored, "\n")[1:] {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "codex: {") {
			t.Fatalf("instruction stored in an unsafe layout:\n%s", stored)
		}
	}
}

func TestPatchConfigYAMLPathKeepsScalarTypes(t *testing.T) {
	body := `{"operations":[{"path":["custom-section","typed"],"value":{"s-bool":"true","s-int":"123","s-null":"null","s-empty":"","int":123,"bool":true,"null":null,"list":[],"map":{}},"replace":true}]}`
	response, stored := patchFixture(t, pathPatchFixture, body)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	var parsed struct {
		Custom map[string]any `yaml:"custom-section"`
	}
	if err := yaml.Unmarshal([]byte(stored), &parsed); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"s-bool": "true", "s-int": "123", "s-null": "null", "s-empty": "", "int": 123, "bool": true, "null": nil, "list": []any{}, "map": map[string]any{}}
	if got := parsed.Custom["typed"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("typed = %#v\nwant %#v", got, want)
	}
}

func TestPatchConfigYAMLPathAcceptsSlashInModelKey(t *testing.T) {
	response, stored := patchFixture(t, pathPatchFixture, `{"operations":[{"path":["reasoning-policy","codex","models","nvidia/llama-3.3"],"value":"low","replace":true}]}`)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	if !strings.Contains(stored, "      gpt-luna: medium\n      nvidia/llama-3.3: low\n") {
		t.Fatalf("model key not stored:\n%s", stored)
	}
}

func TestPatchConfigYAMLPathRejectedRequestsDoNotWrite(t *testing.T) {
	cases := map[string]struct {
		body string
		code int
	}{
		"overlapping paths": {`{"operations":[{"path":["custom-section"],"delete":true},{"path":["custom-section","flag"],"value":true}]}`, http.StatusBadRequest},
		"delete missing":    {`{"operations":[{"path":["custom-section","missing"],"delete":true}]}`, http.StatusNotFound},
		"path through list": {`{"operations":[{"path":["api-keys","0"],"value":"x"}]}`, http.StatusConflict},
		"empty segment":     {`{"operations":[{"path":["custom-section",""],"value":1}]}`, http.StatusBadRequest},
		"missing value":     {`{"operations":[{"path":["port"]}]}`, http.StatusBadRequest},
		"malformed json":    {`{"operations":[`, http.StatusBadRequest},
		"invalid config":    {`{"operations":[{"path":["port"],"value":"not-a-port","replace":true}]}`, http.StatusUnprocessableEntity},
		"second op fails":   {`{"operations":[{"path":["port"],"value":9000,"replace":true},{"path":["nope"],"delete":true}]}`, http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			response, stored := patchFixture(t, pathPatchFixture, tc.body)
			if response.code != tc.code {
				t.Fatalf("status=%d want %d body=%s", response.code, tc.code, response.body)
			}
			if stored != pathPatchFixture {
				t.Fatalf("rejected request changed config:\n%s", stored)
			}
		})
	}
}

func TestPatchConfigYAMLPathRefusesV8MigrationThatDropsSections(t *testing.T) {
	// Writing a v8-layout document migrates it, and migration comments out
	// sections the v8 layout does not model, including api-key-policies. The
	// PATCH must refuse instead.
	initial := "config-version: 8\nreasoning-policy:\n  codex:\n    default: high\napi-key-policies:\n  - api-key: sk-test\n    rpm: 5\ncustom-section:\n  retained: true\n"
	response, stored := patchFixture(t, initial, `{"operations":[{"path":["reasoning-policy","codex","default"],"value":"low","replace":true}]}`)
	if response.code != http.StatusUnprocessableEntity || !strings.Contains(response.body, "patch_not_preserved") {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	if stored != initial {
		t.Fatalf("refused PATCH changed config:\n%s", stored)
	}
}

func TestPatchConfigYAMLPathReplacingAnchoredNodeKeepsConfigReadable(t *testing.T) {
	initial := "port: 8317\nshared: &shared\n  retained: true\ncustom-section: *shared\n"
	response, stored := patchFixture(t, initial, `{"operations":[{"path":["shared"],"value":{"retained":false},"replace":true}]}`)
	if response.code == http.StatusOK {
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(stored), &parsed); err != nil {
			t.Fatalf("accepted PATCH stored unreadable config: %v\n%s", err, stored)
		}
		if !reflect.DeepEqual(parsed["custom-section"], map[string]any{"retained": true}) {
			t.Fatalf("alias target changed: %#v", parsed["custom-section"])
		}
		return
	}
	if stored != initial {
		t.Fatalf("refused PATCH changed config:\n%s", stored)
	}
}

func TestPatchConfigYAMLPathExpectGuardsAgainstClientMisreads(t *testing.T) {
	initial := "port: 8317\ncustom-section:\n  legacy-octal: 0123\n  big: 9007199254740993\n  ratio: 1.0\n  when: 2024-01-02\n  name: kept\n"
	cases := map[string]struct {
		expect string
		code   int
	}{
		// What a YAML 1.2 parser in the browser reads for these scalars.
		"legacy octal read as decimal": {`{"legacy-octal":123,"big":9007199254740993,"ratio":1,"when":"2024-01-02","name":"kept"}`, http.StatusConflict},
		"integer precision lost":       {`{"legacy-octal":83,"big":9007199254740992,"ratio":1,"when":"2024-01-02","name":"kept"}`, http.StatusConflict},
		"timestamp read as string":     {`{"legacy-octal":83,"big":9007199254740993,"ratio":1,"when":"2024-01-02","name":"kept"}`, http.StatusConflict},
		"different string":             {`{"name":"other"}`, http.StatusConflict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"operations":[{"path":["custom-section"],"value":{"name":"changed"},"replace":true,"expect":` + tc.expect + `}]}`
			response, stored := patchFixture(t, initial, body)
			if response.code != tc.code || !strings.Contains(response.body, "expect_mismatch") {
				t.Fatalf("status=%d body=%s", response.code, response.body)
			}
			if stored != initial {
				t.Fatalf("mismatched expect changed config:\n%s", stored)
			}
		})
	}
}

func TestPatchConfigYAMLPathExpectAcceptsMatchingRead(t *testing.T) {
	// JSON has no 1 vs 1.0 distinction, so 1 must match a stored 1.0.
	initial := "port: 8317\ncustom-section:\n  ratio: 1.0\n  list:\n    - a\n    - null\n  nested:\n    flag: true\n"
	body := `{"operations":[` +
		`{"path":["custom-section","ratio"],"value":2.5,"replace":true,"expect":1},` +
		`{"path":["custom-section","list"],"delete":true,"expect":["a",null]},` +
		`{"path":["custom-section","nested"],"value":{"flag":false},"replace":true,"expect":{"flag":true}}]}`
	response, stored := patchFixture(t, initial, body)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	if want := "port: 8317\ncustom-section:\n  ratio: 2.5\n  nested:\n    flag: false\n"; stored != want {
		t.Fatalf("stored:\n%s\nwant:\n%s", stored, want)
	}
}

func TestPatchConfigYAMLPathExpectForMissingPathConflicts(t *testing.T) {
	response, stored := patchFixture(t, pathPatchFixture, `{"operations":[{"path":["custom-section","absent"],"value":1,"replace":true,"expect":0}]}`)
	if response.code != http.StatusConflict || stored != pathPatchFixture {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
}
