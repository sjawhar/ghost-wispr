package server

import "testing"

func TestOpenAPISpecDocumentsRetryRefinementAccepted(t *testing.T) {
	spec := OpenAPISpec()
	responses, _ := dig(t, spec, "paths", "/api/sessions/{id}/retry-refinement", "post", "responses").(map[string]any)
	if _, found := responses["202"]; !found {
		t.Fatalf("retry refinement responses = %#v, want 202 Accepted", responses)
	}
	if _, found := responses["200"]; found {
		t.Fatalf("retry refinement responses = %#v, must not document synchronous 200", responses)
	}
}
