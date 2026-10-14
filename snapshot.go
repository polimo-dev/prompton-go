package prompton

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion is the newest prompt document schema this SDK reads. A deployment revision is
// a pin — one model plus one pinned prompt version per prompt name — not a
// router: schema 8 replaces public resource UUID references with readable keys,
// and schema 9 is the flat single-prompt endpoint shape normalized at decode.
const SchemaVersion = 1

// Kind is what a prompt calls: a chat completion, a text completion, or an
// embedding.
type Kind string

// The prompt kinds.
const (
	KindChat      Kind = "chat"
	KindDecision  Kind = "decision"
	KindText      Kind = "text"
	KindEmbedding Kind = "embedding"
)

// Message is one chat message of a prompt version, before or after rendering.
type Message struct {
	Role       string                   `json:"role,omitempty"`
	Type       string                   `json:"type,omitempty"`
	Content    string                   `json:"content,omitempty"`
	Name       string                   `json:"name,omitempty"`
	ToolCallID string                   `json:"tool_call_id,omitempty"`
	ToolCalls  []map[string]interface{} `json:"tool_calls,omitempty"`
	Extra      map[string]interface{}   `json:"-"`

	rawContent interface{}
	hasContent bool
}

// ContentValue returns the exact decoded JSON content. For messages created through the public
// constructor shape it returns Content.
func (m Message) ContentValue() interface{} {
	if m.hasContent {
		return m.rawContent
	}
	return m.Content
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["role"].(string); ok {
		m.Role = v
	}
	if v, ok := raw["type"].(string); ok {
		m.Type = v
	}
	if v, ok := raw["name"].(string); ok {
		m.Name = v
	}
	if v, ok := raw["tool_call_id"].(string); ok {
		m.ToolCallID = v
	}
	if calls, ok := raw["tool_calls"].([]interface{}); ok {
		m.ToolCalls = make([]map[string]interface{}, 0, len(calls))
		for _, call := range calls {
			if callMap, ok := call.(map[string]interface{}); ok {
				m.ToolCalls = append(m.ToolCalls, callMap)
			}
		}
	}
	if content, ok := raw["content"]; ok {
		m.hasContent = true
		m.rawContent = content
		if s, ok := content.(string); ok {
			m.Content = s
		}
	}
	m.Extra = map[string]interface{}{}
	for key, value := range raw {
		switch key {
		case "role", "type", "content", "name", "tool_call_id", "tool_calls":
		default:
			m.Extra[key] = value
		}
	}
	if len(m.Extra) == 0 {
		m.Extra = nil
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := map[string]interface{}{}
	for key, value := range m.Extra {
		out[key] = value
	}
	if m.Type != "" {
		out["type"] = m.Type
	}
	if m.Role != "" {
		out["role"] = m.Role
	}
	if m.hasContent {
		out["content"] = m.rawContent
	} else if m.Content != "" {
		out["content"] = m.Content
	}
	if m.Name != "" {
		out["name"] = m.Name
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		out["tool_calls"] = m.ToolCalls
	}
	return json.Marshal(out)
}

// Variable is one entry of a prompt's declared variables.
type Variable struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required"`
	Description string      `json:"description,omitempty"`
	Example     interface{} `json:"example,omitempty"`
}

// PayloadPolicy says what the SDK may send of a model call's input and output.
type PayloadPolicy struct {
	Mode          string  `json:"mode"`
	SampleRate    float64 `json:"sample_rate"`
	MaxBytes      int     `json:"max_bytes"`
	RetentionDays int     `json:"retention_days"`
	Encrypt       bool    `json:"encrypt"`
}

// Payload modes.
const (
	PayloadFull = "full"
	PayloadHash = "hash"
	PayloadNone = "none"
)

// DefaultMaxBytes is the payload budget a prompt gets when its policy does
// not name one.
const DefaultMaxBytes = 262144

// Prompt is one LLM call site.
type DocumentPrompt struct {
	ID            string                 `json:"id"`
	Key           string                 `json:"-"`
	Kind          Kind                   `json:"kind"`
	Variables     []Variable             `json:"variables"`
	DefaultParams map[string]interface{} `json:"default_params"`
	PayloadPolicy *PayloadPolicy         `json:"payload_policy"`
}

// Deployment is the live pin for one prompt in one environment.
type Deployment struct {
	ID              string                 `json:"id"`
	Prompt          string                 `json:"-"`
	Revision        string                 `json:"revision"`
	ModelID         string                 `json:"model_id"`
	Provider        string                 `json:"provider"`
	Model           string                 `json:"model"`
	PromptVersion   *int                   `json:"prompt_version"`
	Params          map[string]interface{} `json:"params"`
	ProviderOptions map[string]interface{} `json:"provider_options"`
	PromptPins      map[string]string      `json:"prompt_pins"`
	TemplatePins    map[string]string      `json:"template_pins"`
}

// PromptVersion is an immutable prompt template.
type PromptVersion struct {
	ID               string                 `json:"id"`
	PromptID         string                 `json:"prompt_id"`
	PromptTemplateID string                 `json:"prompt_template_id"`
	Prompt           string                 `json:"prompt"`
	Number           int                    `json:"number"`
	Kind             Kind                   `json:"kind"`
	Engine           string                 `json:"engine"`
	Messages         []Message              `json:"messages"`
	Decision         map[string]interface{} `json:"decision,omitempty"`
	Tools            map[string]interface{} `json:"tools,omitempty"`
	TextTemplate     string                 `json:"text_template"`
}

// Model is a catalog entry: the provider and the provider-side model string the
// app sends.
type Model struct {
	ID              string                 `json:"id"`
	Provider        string                 `json:"provider"`
	ModelID         string                 `json:"model_id"`
	Model           string                 `json:"model"`
	DisplayName     string                 `json:"display_name"`
	Metadata        map[string]interface{} `json:"metadata"`
	ProviderOptions map[string]interface{} `json:"provider_options"`
	Capabilities    []string               `json:"capabilities"`
	Status          string                 `json:"status"`
}

// PromptDocument is a decoded schema1 prompt detail or offline bundle for one
// environment.
type PromptDocument struct {
	SchemaVersion  int
	Project        string
	Environment    string
	Prompts        map[string]*DocumentPrompt
	Deployments    map[string]*Deployment
	PromptVersions map[string]*PromptVersion
	Models         map[string]*Model

	// Raw is the exact document the server sent. The ETag is a hash of these
	// bytes, so the disk cache stores them unchanged.
	Raw []byte

	// Warnings records anything unexpected but tolerable in the decoded document.
	Warnings []string
}

type rawPromptDocument struct {
	SchemaVersion  *int                       `json:"schema_version"`
	Project        string                     `json:"project"`
	Environment    string                     `json:"environment"`
	Prompts        map[string]*DocumentPrompt `json:"prompts"`
	Deployments    map[string]*Deployment     `json:"deployments"`
	PromptVersions map[string]*PromptVersion  `json:"prompt_versions"`
	Models         map[string]*Model          `json:"models"`
	Prompt         *singlePrompt              `json:"prompt"`
	Deployment     *Deployment                `json:"deployment"`
	PromptVersion  *PromptVersion             `json:"prompt_version"`
	Model          *Model                     `json:"model"`
}

type singlePrompt struct {
	Key string `json:"key"`
	*DocumentPrompt
}

// UnsupportedSchemaError is returned for any document whose schema_version is
// not exactly the integer this SDK reads.
type UnsupportedSchemaError struct {
	Version int
	Missing bool
}

func (e *UnsupportedSchemaError) Error() string {
	if e.Missing {
		return fmt.Sprintf("prompton: missing prompt document schema_version (this SDK reads v%d)", SchemaVersion)
	}
	return fmt.Sprintf("prompton: unsupported prompt document schema_version %d (this SDK reads v%d)", e.Version, SchemaVersion)
}

// ParsePromptDocument decodes a schema1 prompt detail or offline aggregate bundle.
func ParsePromptDocument(data []byte) (*PromptDocument, error) {
	if hasPromptInputSchema(data) {
		return nil, fmt.Errorf("prompton: prompt metadata must use variables, not input_schema")
	}
	var raw rawPromptDocument
	if err := decodeJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("prompton: invalid prompt document JSON: %w", err)
	}
	if raw.SchemaVersion == nil {
		return nil, &UnsupportedSchemaError{Missing: true}
	}
	version := *raw.SchemaVersion
	if version != SchemaVersion {
		return nil, &UnsupportedSchemaError{Version: version}
	}
	if err := normalizeSchema1Document(data, &raw); err != nil {
		return nil, err
	}
	if raw.Prompts == nil {
		return nil, fmt.Errorf("prompton: schema1 aggregate bundle must include prompts object")
	}
	if raw.Deployments == nil {
		return nil, fmt.Errorf("prompton: schema1 aggregate bundle must include deployments object")
	}
	if raw.PromptVersions == nil {
		return nil, fmt.Errorf("prompton: schema1 aggregate bundle must include prompt_versions object")
	}
	if raw.Models == nil {
		return nil, fmt.Errorf("prompton: schema1 aggregate bundle must include models object")
	}

	snap := &PromptDocument{
		SchemaVersion:  version,
		Project:        raw.Project,
		Environment:    raw.Environment,
		Prompts:        map[string]*DocumentPrompt{},
		Deployments:    map[string]*Deployment{},
		PromptVersions: map[string]*PromptVersion{},
		Models:         map[string]*Model{},
		Raw:            append([]byte(nil), data...),
	}
	for key, uc := range raw.Prompts {
		if uc == nil {
			continue
		}
		if uc.ID != "" {
			return nil, fmt.Errorf("prompton: schema1 prompt %s must not include id", key)
		}
		uc.Key = key
		if uc.DefaultParams == nil {
			uc.DefaultParams = map[string]interface{}{}
		}
		snap.Prompts[key] = uc
	}
	for key, dep := range raw.Deployments {
		if dep == nil {
			if version == 1 {
				return nil, fmt.Errorf("prompton: schema1 deployment %s must be an object", key)
			}
			continue
		}
		if dep.ID != "" || dep.ModelID != "" || dep.PromptPins != nil || dep.TemplatePins != nil {
			return nil, fmt.Errorf("prompton: schema1 deployment %s must not include legacy resource references", key)
		}
		if dep.PromptVersion != nil && *dep.PromptVersion <= 0 {
			return nil, fmt.Errorf("prompton: deployment %s prompt_version must be a positive integer", key)
		}
		if dep.Provider == "" || dep.Model == "" {
			return nil, fmt.Errorf("prompton: schema1 deployment %s needs provider and model", key)
		}
		dep.Prompt = key
		if dep.Params == nil {
			dep.Params = map[string]interface{}{}
		}
		if dep.ProviderOptions == nil {
			dep.ProviderOptions = map[string]interface{}{}
		}
		if dep.ModelID == "" && dep.Provider != "" && dep.Model != "" {
			dep.ModelID = dep.Provider + ":" + dep.Model
		}
		if dep.PromptVersion != nil {
			dep.TemplatePins = map[string]string{DefaultTemplate: fmt.Sprintf("%s@%d", key, *dep.PromptVersion)}
		} else {
			dep.TemplatePins = map[string]string{}
		}
		snap.Deployments[key] = dep
	}
	for id, pv := range raw.PromptVersions {
		if pv == nil {
			continue
		}
		if pv.ID != "" || pv.PromptID != "" || pv.PromptTemplateID != "" {
			return nil, fmt.Errorf("prompton: schema1 prompt_version %s must not include legacy ids", id)
		}
		if err := validateSchema1PromptVersionKey(id, pv); err != nil {
			return nil, err
		}
		snap.PromptVersions[id] = pv
	}
	for id, m := range raw.Models {
		if m == nil {
			continue
		}
		if m.ID != "" || m.ModelID != "" {
			return nil, fmt.Errorf("prompton: schema1 model %s must not include legacy ids", id)
		}
		if err := validateSchema1ModelKey(id, m); err != nil {
			return nil, err
		}
		if m.ModelID == "" {
			m.ModelID = m.Model
		}
		if m.ProviderOptions == nil {
			m.ProviderOptions = map[string]interface{}{}
		}
		snap.Models[id] = m
	}
	if err := validateSchema1References(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func hasPromptInputSchema(data []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false
	}
	if promptRaw, ok := top["prompt"]; ok {
		var prompt map[string]json.RawMessage
		if json.Unmarshal(promptRaw, &prompt) == nil {
			_, found := prompt["input_schema"]
			return found
		}
	}
	if promptsRaw, ok := top["prompts"]; ok {
		var prompts map[string]map[string]json.RawMessage
		if json.Unmarshal(promptsRaw, &prompts) == nil {
			for _, prompt := range prompts {
				if _, found := prompt["input_schema"]; found {
					return true
				}
			}
		}
	}
	return false
}

func normalizeSchema1Document(data []byte, raw *rawPromptDocument) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err == nil {
		if prompts, ok := top["prompts"]; ok {
			var index []json.RawMessage
			if json.Unmarshal(prompts, &index) == nil {
				return fmt.Errorf("prompton: schema1 prompt index is not a loadable configuration")
			}
		}
	}
	if raw.Prompt == nil && raw.Deployment == nil && raw.PromptVersion == nil && raw.Model == nil {
		return nil
	}
	if hasSchema1BulkCollection(data) {
		return fmt.Errorf("prompton: schema1 prompt detail must not include aggregate collections")
	}
	if raw.Prompt == nil || raw.Prompt.DocumentPrompt == nil {
		return fmt.Errorf("prompton: schema1 prompt must be an object")
	}
	if raw.Deployment == nil {
		return fmt.Errorf("prompton: schema1 deployment must be an object")
	}
	if raw.Model == nil {
		return fmt.Errorf("prompton: schema1 model must be an object")
	}
	key := raw.Prompt.Key
	if key == "" {
		return fmt.Errorf("prompton: schema1 prompt.key is required")
	}
	if raw.Model.Provider == "" || raw.Model.Model == "" {
		return fmt.Errorf("prompton: schema1 model provider and model are required")
	}
	if raw.Deployment.Provider != raw.Model.Provider || raw.Deployment.Model != raw.Model.Model {
		return fmt.Errorf("prompton: schema1 deployment provider/model must match model")
	}
	if raw.PromptVersion == nil {
		return fmt.Errorf("prompton: schema1 prompt_version must be an object")
	}
	if raw.PromptVersion.Prompt != key {
		return fmt.Errorf("prompton: schema1 prompt_version.prompt must match prompt.key")
	}
	if raw.PromptVersion.Number <= 0 {
		return fmt.Errorf("prompton: schema1 prompt_version.number must be a positive integer")
	}
	if raw.Deployment.PromptVersion == nil || *raw.Deployment.PromptVersion != raw.PromptVersion.Number {
		return fmt.Errorf("prompton: schema1 deployment.prompt_version must match prompt_version.number")
	}
	raw.PromptVersions = map[string]*PromptVersion{
		fmt.Sprintf("%s@%d", key, raw.PromptVersion.Number): raw.PromptVersion,
	}
	raw.Prompts = map[string]*DocumentPrompt{key: raw.Prompt.DocumentPrompt}
	raw.Deployments = map[string]*Deployment{key: raw.Deployment}
	raw.Models = map[string]*Model{raw.Model.Provider + ":" + raw.Model.Model: raw.Model}
	return nil
}

func hasSchema1BulkCollection(data []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}
	for _, key := range []string{"prompts", "prompts", "deployments", "prompt_versions", "models"} {
		if _, ok := fields[key]; ok {
			return true
		}
	}
	return false
}

func validateSchema1PromptVersionKey(key string, pv *PromptVersion) error {
	if pv.Prompt == "" || pv.Number <= 0 {
		return fmt.Errorf("prompton: schema1 prompt_version %q needs prompt and positive number fields", key)
	}
	expected := fmt.Sprintf("%s@%d", pv.Prompt, pv.Number)
	if key != expected {
		return fmt.Errorf("prompton: schema1 prompt_versions key %q must match prompt and number fields", key)
	}
	return nil
}

func validateSchema1ModelKey(key string, m *Model) error {
	provider, model, ok := strings.Cut(key, ":")
	if !ok || provider == "" || model == "" || m.Provider != provider || m.Model != model {
		return fmt.Errorf("prompton: schema1 models key %q must match provider and model fields", key)
	}
	return nil
}

func validateSchema1References(snap *PromptDocument) error {
	for key, dep := range snap.Deployments {
		if dep == nil {
			return fmt.Errorf("prompton: schema1 deployment %s must be an object", key)
		}
		uc := snap.Prompts[key]
		if uc == nil {
			return fmt.Errorf("prompton: schema1 deployment %s references unknown prompt", key)
		}
		if snap.Models[dep.ModelID] == nil {
			return fmt.Errorf("prompton: schema1 deployment %s references unknown model %s", key, dep.ModelID)
		}
		if uc.Kind == KindEmbedding {
			continue
		}
		if len(dep.TemplatePins) == 0 {
			return fmt.Errorf("prompton: schema1 deployment %s must pin at least one prompt version", key)
		}
		if dep.PromptVersion == nil || *dep.PromptVersion <= 0 {
			return fmt.Errorf("prompton: deployment %s prompt_version must be a positive integer", key)
		}
		defaultVersionKey := fmt.Sprintf("%s@%d", key, *dep.PromptVersion)
		versionKey := dep.TemplatePins[DefaultTemplate]
		if versionKey != defaultVersionKey {
			return fmt.Errorf("prompton: schema1 deployment %s default prompt pin must match prompt_version", key)
		}
		version := snap.PromptVersions[versionKey]
		if version == nil {
			return fmt.Errorf("prompton: schema1 deployment %s references unknown prompt version %s", key, versionKey)
		}
		if version.Prompt != key {
			return fmt.Errorf("prompton: schema1 deployment %s prompt version references prompt %s", key, version.Prompt)
		}
	}
	return nil
}

// TemplateNames lists the template names the live revision of a prompt pins.
func (s *PromptDocument) TemplateNames(prompt string) []string {
	dep := s.Deployments[prompt]
	if dep == nil {
		return []string{}
	}
	names := make([]string, 0, len(dep.TemplatePins))
	for name := range dep.TemplatePins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mergeParams shallow-merges override on top of base, right side wins. A nested
// map on the right replaces the left whole, and an override value of null is
// kept as null rather than deleting the key — apps rely on sending
// "only": null to clear a provider restriction.
func mergeParams(base, override map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}
