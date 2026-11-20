package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/jsonschema"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// Status is how a provider signs in and whether that worked when last checked.
type Status struct {
	Name string `json:"name"`
	API  string `json:"api"`
	URL  string `json:"url"`
	// Auth is the sign-in kind, and SignIn says it in words.
	Auth   string `json:"auth"`
	SignIn string `json:"sign_in"`
	// KeyEnv is an api_key provider's variable, and KeySet whether it holds
	// a key.
	KeyEnv string `json:"key_env,omitempty"`
	KeySet bool   `json:"key_set"`
	// Checked says a check ran. Asked says the provider's answers proved
	// every model its roles call, or refused one: false when a minted or
	// exchanged token is the check's only proof, when a model of a Vertex AI
	// provider cannot be counted, when the check reads only the provider's
	// account (Deepgram's projects, the ElevenLabs user, Cloud
	// Text-to-Speech's voices) while a role calls a model on it, or when a
	// role's model has no cheap call (a speech model), even after the
	// provider answered other reads. OK says it signed in and, when asked,
	// that the provider took the credential.
	Checked   bool      `json:"checked"`
	Asked     bool      `json:"asked"`
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}

// checkTimeout bounds one provider's check.
const checkTimeout = 10 * time.Second

// SignIn says how a provider signs in, in words for the settings page.
func SignIn(p *config.Provider, credentialsFile string) string {
	switch p.Auth.Kind {
	case config.AuthAPIKey:
		return "API key from " + p.Auth.Env
	case config.AuthGoogle:
		if credentialsFile != "" {
			return "Google workload identity (google_credentials_file)"
		}
		return "Google Application Default Credentials"
	case config.AuthGoogleIDToken:
		return "Google ID token for audience " + p.Auth.Audience
	case config.AuthOpenAIWIF:
		return "OpenAI workload identity: an OpenAI access token in exchange for the unit's Google ID token"
	default:
		return p.Auth.Kind
	}
}

func (p *Provider) status() Status {
	s := Status{
		Name: p.Name, API: p.API, URL: p.BaseURL(), Auth: p.Auth.Kind,
		SignIn: SignIn(&p.Provider, p.credentialsFile),
	}
	if p.Auth.Kind == config.AuthAPIKey {
		s.KeyEnv, s.KeySet = p.Auth.Env, p.apiKey() != ""
	}
	return s
}

// Check signs the provider in and, where its API has a cheap read that the
// credential must pass, makes it: a model list, Deepgram's projects, Cloud
// Text-to-Speech's voices, or Vertex AI's count of a short text's tokens for
// each model a role calls on it, which costs nothing and needs only the
// predict permission a unit's identity holds. A refusal of any read fails
// the check, naming the model a count asked about. The provider works only
// once the check proved every model its roles call: a Vertex AI provider
// with a model it cannot count (an embedding model's 400), and one whose
// read is its account alone (Deepgram, ElevenLabs, Cloud Text-to-Speech)
// while a role calls a model on it, only signed in. A provider whose read is
// its model list must also list each model its roles call and answer one
// cheap call of each (checkModels); a Gemini API provider whose list answers
// 404, as a model gateway's Google routes do, instead answers one
// generateContent of a token of each chat model (generateCall). An OpenAI
// token exchanged is itself the check of OpenAI workload identity, as a
// Google token minted is of a Vertex AI provider none of whose models can be
// counted. It never reports a credential, nor what the provider answered
// beyond its status and code.
func (p *Provider) Check(ctx context.Context) Status {
	s := p.status()
	s.Checked, s.CheckedAt = true, p.set.opts.Now()
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	credential, err := p.Credential(ctx)
	if err != nil {
		// Credential's errors are this package's, googleauth's and
		// openaiwif's, which already withhold what a provider answered.
		s.Message = err.Error()
		return s
	}
	host := p.BaseURL()
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	// The check's credential goes where a provider's requests do: a
	// redirect to another origin stops it.
	client := *p.set.opts.Client
	client.CheckRedirect = p.checkRedirect(client.CheckRedirect)
	uncounted := 0
	var counted []string // the models a count proved
	reads := p.checkReads()
	for i := range reads {
		read := &reads[i]
		status, body, err := p.ask(ctx, &client, read, credential)
		if err != nil {
			s.Message = err.Error()
			return s
		}
		if read.perModel && status == http.StatusBadRequest {
			// This model cannot be counted (an embedding model's), which
			// says nothing of the credential: ask about the next.
			uncounted = status
			continue
		}
		if read.perModel && status == http.StatusNotFound {
			// No model of that name at this location, or no such
			// location: every call of the model would fail.
			s.Asked = true
			s.Message = fmt.Sprintf("%s answered HTTP 404 to a count of %s: check the location and the model.", host, read.model)
			return s
		}
		if read.list != nil && status == http.StatusNotFound && p.API == config.APIGemini && len(p.uses) > 0 {
			// A model gateway's Google routes (LiteLLM's) serve no model
			// list and no token count: each chat model is proved by one
			// generateContent of a token instead (generateCall).
			var proved bool
			s.OK, proved, s.Message = p.callModels(ctx, &client, credential, host, p.generateCall)
			s.Asked = proved || !s.OK
			return s
		}
		// A count names the model it asked about: another model of the
		// provider may have been counted before it.
		what := ""
		if read.perModel {
			what = " to a count of " + read.model
		}
		switch {
		case status == http.StatusForbidden && p.API == config.APIVertex:
			// Vertex AI answers 403 to a project that does not exist, or
			// a location it does not serve, as to a credential it refuses.
			s.Asked = true
			s.Message = fmt.Sprintf("%s refused the request (HTTP 403)%s: check the sign-in, and the project and location.", host, what)
			return s
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			// The answer's text can quote what it was sent; its status alone.
			s.Asked = true
			s.Message = fmt.Sprintf("%s refused the credential (HTTP %d)%s.", host, status, what)
			return s
		case status/100 != 2:
			s.Asked = true
			s.Message = fmt.Sprintf("Signed in, but %s answered HTTP %d%s.", host, status, what)
			return s
		}
		if read.perModel {
			// Counted: the provider works only once each model is.
			counted = append(counted, read.model)
			continue
		}
		if read.list != nil && len(p.uses) > 0 {
			var proved bool
			s.OK, proved, s.Message = p.checkModels(ctx, &client, credential, host, read, body)
			// Works needs a call of each model a role calls; a model no
			// cheap call proves (speech, speech-to-speech) leaves a provider
			// that answered everything else at Signed in. A refusal was an
			// answer.
			s.Asked = proved || !s.OK
			return s
		}
		// The read is of the provider's account (Deepgram's projects, the
		// ElevenLabs user, Cloud Text-to-Speech's voices), or of a model
		// list no role needs: it proves the credential and no model, so the
		// provider works only when no role calls a model on it.
		s.OK, s.Message = true, fmt.Sprintf("Signed in: %s accepted the credential.", host)
		if models := p.models(); len(models) > 0 {
			s.Message += " The check makes no call of " + strings.Join(models, ", ") + "."
		} else {
			s.Asked = true
		}
		return s
	}
	if len(counted) > 0 {
		// Vertex AI: the provider works once every model was counted. One
		// it cannot count (an embedding model's 400), or a model named with
		// a slash, which no count asks about, is unproved.
		s.OK, s.Message = true, fmt.Sprintf("Signed in: %s accepted the credential and counted the tokens of %s.", host, strings.Join(counted, ", "))
		var unproved []string
		for _, model := range p.models() {
			if !slices.Contains(counted, model) {
				unproved = append(unproved, model)
			}
		}
		if len(unproved) > 0 {
			s.Message += " The check proves no call of " + strings.Join(unproved, ", ") + "."
		} else {
			s.Asked = true
		}
		return s
	}
	s.OK, s.Message = true, "Signed in: "+map[string]string{
		config.AuthGoogle:        "Google issued an access token.",
		config.AuthGoogleIDToken: "Google issued an ID token.",
		config.AuthOpenAIWIF:     "OpenAI exchanged the Google ID token for an access token.",
		config.AuthAPIKey:        "the key is set.",
	}[p.Auth.Kind]
	if uncounted != 0 {
		s.Message += fmt.Sprintf(" %s counts the tokens of none of the models its roles call (HTTP %d), so the check could ask it nothing.", host, uncounted)
	}
	return s
}

// models are the models the provider's roles call on it, each once, in the
// roles' order.
func (p *Provider) models() []string {
	var models []string
	for _, u := range p.uses {
		if !slices.Contains(models, u.Model) {
			models = append(models, u.Model)
		}
	}
	return models
}

// checkRead is a read a check makes. perModel says it asks about model, so
// that a 400 says only that this model cannot be asked about. list reads
// its answer as a page of the provider's model list. keep says the check
// reads the answer's body.
type checkRead struct {
	method, url, body, model string
	perModel, keep           bool
	list                     modelList
}

// countTokensBody is the text whose tokens a Vertex AI check counts.
const countTokensBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

// checkReads are the reads a check makes, in order until one decides it;
// none when minting the credential is the check.
func (p *Provider) checkReads() []checkRead {
	base := p.BaseURL()
	get := func(path string) []checkRead { return []checkRead{{method: http.MethodGet, url: base + path}} }
	list := func(path string, list modelList) []checkRead {
		return []checkRead{{method: http.MethodGet, url: base + path, keep: len(p.uses) > 0, list: list}}
	}
	switch p.Auth.Kind {
	case config.AuthOpenAIWIF:
		return nil
	case config.AuthGoogle:
		switch p.API {
		case config.APIVertex:
			return p.countTokensReads(base)
		case config.APIGoogleTTS:
			return get("/v1/voices?languageCode=en-US")
		}
		return nil
	}
	switch p.API {
	case config.APIOpenAI:
		return list("/models", openAIModels)
	case config.APIAnthropic:
		return list("/v1/models?limit=1000", anthropicModels)
	case config.APIGemini:
		return list("/v1beta/models?pageSize=1000", geminiModels)
	case config.APIDeepgram:
		return get("/v1/projects")
	case config.APIElevenLabs:
		return get("/v1/user")
	case config.APIGoogleTTS:
		return get("/v1/voices?languageCode=en-US")
	}
	return nil
}

// countTokensReads count a short text's tokens for each Google model the
// provider's roles call on it. A model named with a slash is another
// publisher's or a whole resource name, which this path does not take.
func (p *Provider) countTokensReads(base string) []checkRead {
	if p.Project == "" {
		return nil
	}
	var reads []checkRead
	var counted []string
	for _, use := range p.uses {
		if strings.Contains(use.Model, "/") || slices.Contains(counted, use.Model) {
			continue
		}
		counted = append(counted, use.Model)
		reads = append(reads, checkRead{
			method: http.MethodPost,
			url: fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:countTokens",
				base, url.PathEscape(p.Project), url.PathEscape(cmp.Or(p.Location, config.DefaultVertexLocation)), url.PathEscape(use.Model)),
			body:     countTokensBody,
			model:    use.Model,
			perModel: true,
		})
	}
	return reads
}

// modelList reads page, one page of a provider's model list read from
// pageURL: the models it names, as the provider's roles name them, and the
// URL of the next page, "" after the last. ok is false when page is no model
// list.
type modelList func(page []byte, pageURL string) (models []string, next string, ok bool)

// openAIModels reads the OpenAI API's list, which a model gateway such as
// LiteLLM answers with the models the caller's team may call: one page.
func openAIModels(page []byte, _ string) ([]string, string, bool) {
	var list struct {
		Data *[]struct{ ID string } `json:"data"`
	}
	if json.Unmarshal(page, &list) != nil || list.Data == nil {
		return nil, "", false
	}
	models := make([]string, 0, len(*list.Data))
	for _, m := range *list.Data {
		models = append(models, m.ID)
	}
	return models, "", true
}

// anthropicModels reads a page of the Anthropic API's list; the next page
// starts after its last id.
func anthropicModels(page []byte, pageURL string) ([]string, string, bool) {
	var list struct {
		Data    *[]struct{ ID string } `json:"data"`
		HasMore bool                   `json:"has_more"`
		LastID  string                 `json:"last_id"`
	}
	if json.Unmarshal(page, &list) != nil || list.Data == nil {
		return nil, "", false
	}
	models := make([]string, 0, len(*list.Data))
	for _, m := range *list.Data {
		models = append(models, m.ID)
	}
	next := ""
	if list.HasMore && list.LastID != "" {
		next = withQuery(pageURL, "after_id", list.LastID)
	}
	return models, next, true
}

// geminiModels reads a page of the Gemini API's list, whose models are
// named models/<model>; the next page is its nextPageToken's.
func geminiModels(page []byte, pageURL string) ([]string, string, bool) {
	var list struct {
		Models *[]struct {
			Name string `json:"name"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
	}
	if json.Unmarshal(page, &list) != nil || list.Models == nil {
		return nil, "", false
	}
	models := make([]string, 0, len(*list.Models))
	for _, m := range *list.Models {
		models = append(models, strings.TrimPrefix(m.Name, "models/"))
	}
	next := ""
	if list.NextPageToken != "" {
		next = withQuery(pageURL, "pageToken", list.NextPageToken)
	}
	return models, next, true
}

// withQuery is rawURL with its query parameter key set to value.
func withQuery(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// maxModelPages bounds the pages of a model list a check reads.
const maxModelPages = 20

// checkModels finds each model the provider's roles call in its model list,
// whose first page is page, and makes one call of each that its role
// decides (modelCall): the provider works only when it lists each model and
// answers each call. A model gateway admits a caller to its sign-in and to
// each model separately, so the list read alone proves the first. It
// returns whether the provider works, whether a call proved every model the
// roles call (a speech model, which no cheap call proves, does not), and what
// the check found.
func (p *Provider) checkModels(ctx context.Context, client *http.Client, credential, host string, read *checkRead, page []byte) (works, proved bool, _ string) {
	listed := map[string]bool{}
	pageURL := read.url
	for range maxModelPages {
		models, next, ok := read.list(page, pageURL)
		if !ok {
			return false, false, host + " accepted the credential but did not answer with a model list."
		}
		for _, m := range models {
			listed[m] = true
		}
		if next == "" || !slices.ContainsFunc(p.uses, func(u config.ModelUse) bool { return !listed[u.Model] }) {
			break
		}
		status, body, err := p.ask(ctx, client, &checkRead{method: http.MethodGet, url: next, keep: true}, credential)
		if err != nil {
			return false, false, err.Error()
		}
		if status/100 != 2 {
			return false, false, fmt.Sprintf("Signed in, but %s answered HTTP %d to a page of its model list.", host, status)
		}
		page, pageURL = body, next
	}
	// A vendor's own API lists dated models and not the aliases a role may
	// name (claude-…-latest, an OpenAI family alias), so there a model it
	// does not list is decided by its call; a gateway lists exactly what
	// the caller may call.
	if p.BaseURL() != defaultURLs[p.API] {
		var missing []string
		for _, u := range p.uses {
			if !listed[u.Model] && !slices.Contains(missing, u.Model) {
				missing = append(missing, u.Model)
			}
		}
		if len(missing) > 0 {
			return false, false, fmt.Sprintf("%s accepted the credential but lists no %s for it.", host, strings.Join(missing, " or "))
		}
	}
	return p.callModels(ctx, client, credential, host, p.modelCall)
}

// callModels makes one call of each model the provider's roles call, the
// one callOf gives (one cheap call of the model for its role; false for a
// model no cheap call proves): the provider works only when it answers each.
// It returns whether the provider works, whether a call proved every model
// the roles call, and what the check found, naming a refused model and the
// refusal's status and code alone.
func (p *Provider) callModels(ctx context.Context, client *http.Client, credential, host string, callOf func(config.ModelUse) (checkRead, bool)) (works, proved bool, _ string) {
	var called []string
	var unproved []config.ModelUse // the uses no cheap call proves
	var made []string              // each call made, by its URL and body
	for _, u := range p.uses {
		call, ok := callOf(u)
		if !ok {
			unproved = append(unproved, u)
			continue
		}
		if slices.Contains(made, call.url+call.body) {
			continue
		}
		made = append(made, call.url+call.body)
		status, body, err := p.ask(ctx, client, &call, credential)
		if err != nil {
			return false, false, err.Error()
		}
		code := refusalCode(body)
		switch {
		case status == http.StatusUnauthorized:
			return false, false, fmt.Sprintf("%s refused the credential (HTTP 401).", host)
		case status == http.StatusForbidden:
			answer := "HTTP 403"
			if code != "" {
				answer += " " + code
			}
			return false, false, fmt.Sprintf("%s refused %s for this sign-in (%s).", host, u.Model, answer)
		case status/100 != 2:
			return false, false, fmt.Sprintf("Signed in, but %s answered HTTP %d%s to a call of %s.", host, status, parenthesized(code), u.Model)
		}
		if !slices.Contains(called, u.Model) {
			called = append(called, u.Model)
		}
	}
	message := "Signed in: " + host + " accepted the credential"
	if len(called) > 0 {
		message += " and answered a call of " + strings.Join(called, ", ")
	}
	message += "."
	// A model one role's call proved was called; another role's use of it
	// that no cheap call proves (text-to-speech on a chat model) is still
	// unproved, and says so by its role.
	var uncalled, unprovedUses []string
	for _, u := range unproved {
		switch {
		case !slices.Contains(called, u.Model):
			if !slices.Contains(uncalled, u.Model) {
				uncalled = append(uncalled, u.Model)
			}
		case !slices.Contains(unprovedUses, u.Model+" for "+u.Role.Key):
			unprovedUses = append(unprovedUses, u.Model+" for "+u.Role.Key)
		}
	}
	if len(uncalled) > 0 {
		message += " The check makes no call of " + strings.Join(uncalled, ", ") + "."
	}
	if len(unprovedUses) > 0 {
		message += " No cheap call proves " + strings.Join(unprovedUses, ", ") + "."
	}
	return true, len(unproved) == 0, message
}

// generateCall is the call a check makes of a model of a Gemini API
// provider whose model list answered 404, as a model gateway's Google routes
// (LiteLLM's) serve no model list and no token count: for a chat model, one
// generateContent of a one-word user text and at most one output token; for
// an embedding model, modelCall's. A speech model has no cheap call there.
func (p *Provider) generateCall(use config.ModelUse) (checkRead, bool) {
	switch use.Role.Key {
	case config.RoleSummarization.Key, config.RoleConcierge.Key:
		path := "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(use.Model, "models/")) + ":generateContent"
		return checkRead{method: http.MethodPost, url: p.BaseURL() + path, body: generateCallBody, keep: true}, true
	case config.RoleEmbedding.Key:
		return p.modelCall(use)
	}
	return checkRead{}, false
}

// generateCallBody is the generateContent a check makes of a chat model at
// a Gemini API gateway.
const generateCallBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1}}`

// modelCall is the call a check makes of a model for the role that calls
// it, the cheapest each API has: a count of a short text's tokens where the
// API counts them for nothing (Anthropic, the Gemini API), else a chat
// completion of one token, and for an embedding model an embedding of one
// word. Through a model gateway that embedding costs one input of the
// gateway's per-minute budget (300 a minute on the office gateway's route);
// the settings page checks a provider once in 5 minutes unless someone asks
// it to check again. ok is false for a model no cheap call proves: a speech
// model, a speech-to-speech one.
func (p *Provider) modelCall(use config.ModelUse) (checkRead, bool) {
	base, model := p.BaseURL(), use.Model
	post := func(path string, body any) (checkRead, bool) {
		data, _ := json.Marshal(body)
		return checkRead{method: http.MethodPost, url: base + path, body: string(data), keep: true}, true
	}
	user := []map[string]string{{"role": "user", "content": "hi"}}
	chat := use.Role.Key == config.RoleSummarization.Key || use.Role.Key == config.RoleConcierge.Key
	embedding := use.Role.Key == config.RoleEmbedding.Key
	switch p.API {
	case config.APIOpenAI:
		switch {
		case embedding:
			return post("/embeddings", map[string]any{"model": model, "input": "hello"})
		case chat:
			// max_completion_tokens, which OpenAI's reasoning models take
			// and refuse max_tokens for; LiteLLM maps it for each vendor.
			request := map[string]any{"model": model, "messages": user, "max_completion_tokens": 1}
			if use.Role.Key == config.RoleSummarization.Key {
				// The summarizer asks for structured output, so the call
				// does too, as its client sends it (llm's CompleteJSON): a
				// provider that refuses the summary's schema refuses every
				// summary.
				request["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{
					"name": "response", "schema": summaryShapedSchema, "strict": jsonschema.Strict(summaryShapedSchema),
				}}
			}
			return post("/chat/completions", request)
		}
	case config.APIAnthropic:
		if chat {
			return post("/v1/messages/count_tokens", map[string]any{"model": model, "messages": user})
		}
	case config.APIGemini:
		path := "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/"))
		if embedding {
			return post(path+":embedContent", map[string]any{"content": map[string]any{"parts": []map[string]string{{"text": "hello"}}}})
		}
		return post(path+":countTokens", map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "hi"}}}}})
	}
	return checkRead{}, false
}

// summaryShapedSchema is the summary's schema (summary.StructuredSchema)
// without its descriptions: an object of a title, a summary, and speakers as
// a list of {id, name, confidence}, each property required, which strict
// structured output takes. The check asks for it where the summarizer asks
// for the summary's; providers cannot import summary, so internal/summary's
// TestTheProviderCheckAsksForTheSummarysSchema holds this copy to it.
var summaryShapedSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title":   map[string]any{"type": "string"},
		"summary": map[string]any{"type": "string"},
		"speakers": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":         map[string]any{"type": "string"},
					"name":       map[string]any{"type": "string"},
					"confidence": map[string]any{"type": "string", "enum": []string{"mentioned", "inferred"}},
				},
				"required":             []string{"confidence", "id", "name"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"speakers", "summary", "title"},
	"additionalProperties": false,
}

// refusalCode is the code of a provider's refusal: its error's code, type
// or status (OpenAI and LiteLLM, Anthropic, Google), the first that is a
// plain token. Nothing else of the body is kept: it can quote the request.
func refusalCode(body []byte) string {
	var answer struct {
		Error struct {
			Code   any    `json:"code"`
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return ""
	}
	code, _ := answer.Error.Code.(string)
	return cmp.Or(providererr.Token(code), providererr.Token(answer.Error.Type), providererr.Token(answer.Error.Status))
}

// parenthesized is " (code)", or "" for no code.
func parenthesized(code string) string {
	if code == "" {
		return ""
	}
	return " (" + code + ")"
}

// modelListLimit bounds a model list page a check reads; other answers it
// reads are a few hundred bytes.
const modelListLimit = 16 << 20

// ask makes read with credential and returns the provider's status and,
// when read keeps it, the answer's body. Its errors withhold what the
// provider sent.
func (p *Provider) ask(ctx context.Context, client *http.Client, read *checkRead, credential string) (int, []byte, error) {
	var body io.Reader = http.NoBody
	if read.body != "" {
		body = strings.NewReader(read.body)
	}
	req, err := http.NewRequestWithContext(ctx, read.method, read.url, body)
	if err != nil {
		return 0, nil, err
	}
	p.SetAuth(req.Header, credential)
	if read.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.API == config.APIAnthropic {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	resp, err := client.Do(req)
	if err != nil {
		var away *originError
		if errors.As(err, &away) {
			return 0, nil, away // names the provider's origin alone, not the redirect's target
		}
		return 0, nil, providererr.From(p.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !read.keep {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, nil, nil
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, modelListLimit))
	if err != nil {
		return 0, nil, providererr.From(p.Name, err)
	}
	return resp.StatusCode, answer, nil
}

// Statuses returns every provider's status, by name. A provider checked
// within maxAge keeps its last check; the rest are checked now, together.
// maxAge 0 checks every one. A check is cached only while no key was saved
// for its provider since it began (SetKeys): a check of the replaced key
// that ends after the saved key's never replaces the saved key's status.
func (s *Set) Statuses(ctx context.Context, maxAge time.Duration) []Status {
	now := s.opts.Now()
	var wg sync.WaitGroup
	results := make(map[string]Status, len(s.providers))
	var mu sync.Mutex
	for name, p := range s.providers {
		s.mu.Lock()
		last, ok := s.status[name]
		gen := s.keyGen[name]
		s.mu.Unlock()
		if ok && maxAge > 0 && now.Sub(last.CheckedAt) < maxAge {
			mu.Lock()
			results[name] = last
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			checked := p.Check(ctx)
			mu.Lock()
			results[name] = checked
			mu.Unlock()
			s.mu.Lock()
			if s.keyGen[name] == gen {
				s.status[name] = checked
			}
			s.mu.Unlock()
		}()
	}
	wg.Wait()
	out := make([]Status, 0, len(results))
	for _, name := range slices.Sorted(maps.Keys(results)) {
		out = append(out, results[name])
	}
	return out
}
