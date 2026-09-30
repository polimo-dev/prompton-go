package prompton

import (
	"encoding/json"
	"fmt"
	"sort"
)

// SchemaVersion is the newest use-case document schema this SDK reads. A deployment revision is
// a pin — one model plus one pinned prompt version per prompt name — not a
// router: schema 7 adds provider-visible chat tools and full native messages.
const SchemaVersion = 7

// Kind is what a use case calls: a chat completion, a text completion, or an
// embedding.
type Kind string

// The three use case kinds.
const (
	KindChat      Kind = "chat"
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
	} else if m.Content != "" || m.Type != "slot" {
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

// InputVariable is one entry of a use case's declared input schema.
type InputVariable struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
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

// DefaultMaxBytes is the payload budget a use case gets when its policy does
// not name one.
const DefaultMaxBytes = 262144

// UseCase is one LLM call site.
type DocumentUseCase struct {
	ID            string                 `json:"id"`
	Key           string                 `json:"-"`
	Kind          Kind                   `json:"kind"`
	InputSchema   []InputVariable        `json:"input_schema"`
	DefaultParams map[string]interface{} `json:"default_params"`
	PayloadPolicy *PayloadPolicy         `json:"payload_policy"`
}

// Deployment is the live pin for one use case in one environment.
type Deployment struct {
	ID              string                 `json:"id"`
	UseCase         string                 `json:"-"`
	Revision        string                 `json:"revision"`
	ModelID         string                 `json:"model_id"`
	Params          map[string]interface{} `json:"params"`
	ProviderOptions map[string]interface{} `json:"provider_options"`
	PromptPins      map[string]string      `json:"prompt_pins"`
	TemplatePins    map[string]string      `json:"template_pins"`
}

// PromptVersion is an immutable prompt template.
type PromptVersion struct {
	ID           string                 `json:"id"`
	PromptID     string                 `json:"prompt_id"`
	Number       int                    `json:"number"`
	Engine       string                 `json:"engine"`
	Messages     []Message              `json:"messages"`
	Tools        map[string]interface{} `json:"tools,omitempty"`
	TextTemplate string                 `json:"text_template"`
}

// Model is a catalog entry: the provider and the provider-side model string the
// app sends.
type Model struct {
	ID              string                 `json:"id"`
	Provider        string                 `json:"provider"`
	ModelID         string                 `json:"model_id"`
	DisplayName     string                 `json:"display_name"`
	Metadata        map[string]interface{} `json:"metadata"`
	ProviderOptions map[string]interface{} `json:"provider_options"`
	Capabilities    []string               `json:"capabilities"`
	Status          string                 `json:"status"`
}

// UseCaseDocument is a decoded GET /prompts document: everything live in one
// environment, selected locally with no further network calls.
type UseCaseDocument struct {
	SchemaVersion  int
	Project        string
	Environment    string
	UseCases       map[string]*DocumentUseCase
	Deployments    map[string]*Deployment
	PromptVersions map[string]*PromptVersion
	Models         map[string]*Model

	// Raw is the exact document the server sent. The ETag is a hash of these
	// bytes, so the disk cache stores them unchanged.
	Raw []byte

	// Warnings records anything unexpected but tolerable in the document, such
	// as a newer schema version.
	Warnings []string
}

type rawUseCaseDocument struct {
	SchemaVersion  *int                        `json:"schema_version"`
	Project        string                      `json:"project"`
	Environment    string                      `json:"environment"`
	UseCases       map[string]*DocumentUseCase `json:"use_cases"`
	Prompts        map[string]*DocumentUseCase `json:"prompts"`
	Deployments    map[string]*Deployment      `json:"deployments"`
	PromptVersions map[string]*PromptVersion   `json:"prompt_versions"`
	Models         map[string]*Model           `json:"models"`
}

// UnsupportedSchemaError is returned for any document whose schema_version is
// not exactly the integer this SDK reads.
type UnsupportedSchemaError struct {
	Version int
	Missing bool
}

func (e *UnsupportedSchemaError) Error() string {
	if e.Missing {
		return fmt.Sprintf("prompton: missing use-case document schema_version (this SDK reads v%d)", SchemaVersion)
	}
	return fmt.Sprintf("prompton: unsupported use-case document schema_version %d (this SDK reads v%d)", e.Version, SchemaVersion)
}

// ParseUseCaseDocument decodes a GET /prompts body.
func ParseUseCaseDocument(data []byte) (*UseCaseDocument, error) {
	var raw rawUseCaseDocument
	if err := decodeJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("prompton: invalid use-case document JSON: %w", err)
	}
	if raw.SchemaVersion == nil {
		return nil, &UnsupportedSchemaError{Missing: true}
	}
	version := *raw.SchemaVersion
	if version < 4 || version > SchemaVersion {
		return nil, &UnsupportedSchemaError{Version: version}
	}
	if raw.UseCases == nil {
		raw.UseCases = raw.Prompts
	}
	if raw.UseCases == nil {
		return nil, fmt.Errorf("prompton: use-case document has no use_cases object")
	}

	snap := &UseCaseDocument{
		SchemaVersion:  version,
		Project:        raw.Project,
		Environment:    raw.Environment,
		UseCases:       map[string]*DocumentUseCase{},
		Deployments:    map[string]*Deployment{},
		PromptVersions: map[string]*PromptVersion{},
		Models:         map[string]*Model{},
		Raw:            append([]byte(nil), data...),
	}
	for key, uc := range raw.UseCases {
		if uc == nil {
			continue
		}
		uc.Key = key
		if uc.DefaultParams == nil {
			uc.DefaultParams = map[string]interface{}{}
		}
		snap.UseCases[key] = uc
	}
	for key, dep := range raw.Deployments {
		if dep == nil {
			continue
		}
		dep.UseCase = key
		if dep.Params == nil {
			dep.Params = map[string]interface{}{}
		}
		if dep.ProviderOptions == nil {
			dep.ProviderOptions = map[string]interface{}{}
		}
		if dep.PromptPins == nil {
			dep.PromptPins = dep.TemplatePins
		}
		if dep.PromptPins == nil {
			dep.PromptPins = map[string]string{}
		}
		snap.Deployments[key] = dep
	}
	for id, pv := range raw.PromptVersions {
		if pv == nil {
			continue
		}
		if pv.ID == "" {
			pv.ID = id
		}
		snap.PromptVersions[id] = pv
	}
	for id, m := range raw.Models {
		if m == nil {
			continue
		}
		if m.ID == "" {
			m.ID = id
		}
		if m.ProviderOptions == nil {
			m.ProviderOptions = map[string]interface{}{}
		}
		snap.Models[id] = m
	}
	return snap, nil
}

// PromptNames lists the prompt names the live revision of a use case pins.
func (s *UseCaseDocument) PromptNames(useCase string) []string {
	dep := s.Deployments[useCase]
	if dep == nil {
		return []string{}
	}
	names := make([]string, 0, len(dep.PromptPins))
	for name := range dep.PromptPins {
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
