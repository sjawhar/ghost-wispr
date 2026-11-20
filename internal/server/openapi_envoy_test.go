package server

import (
	"encoding/json"
	"testing"
)

// TestOpenAPIEnvoyEventCarriesNoCredential fails if the documented publish to
// the Envoy hub inherits the API's own Basic Authentication: the unit sends it
// with no credential, which the hub's listener takes over the tailnet. The
// control is the document-wide requirement, which the unit's own routes keep.
func TestOpenAPIEnvoyEventCarriesNoCredential(t *testing.T) {
	data, err := OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Security []map[string][]string `json:"security"`
		Webhooks map[string]struct {
			Post map[string]json.RawMessage `json:"post"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Security) == 0 {
		t.Fatal("control: the spec requires no credential for the unit's own routes")
	}
	security, ok := spec.Webhooks["envoyEvent"].Post["security"]
	if !ok {
		t.Fatal("the envoyEvent publish inherits the document-wide security requirement")
	}
	var requirements []map[string][]string
	if err := json.Unmarshal(security, &requirements); err != nil || len(requirements) != 0 {
		t.Fatalf("envoyEvent security = %s, want []", security)
	}
}
