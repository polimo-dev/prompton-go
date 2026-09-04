package prompton

import (
	"fmt"
	"sort"
)

// SchemaVersion is the snapshot schema this SDK reads. A deployment revision is
// a pin — one model plus one pinned prompt version per prompt name — not a
// router: v3 has no rules, targets, weights or context dimensions.
const SchemaVersion = 3

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
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// InputVariable is one entry of a use case's declared input schema.
type InputVariable struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// PayloadPolicy says what the SDK may send of a generation's input and output.
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
type UseCase struct {
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
	Revision        int                    `json:"revision"`
	ModelID         string                 `json:"model_id"`
	Params          map[string]interface{} `json:"params"`
	ProviderOptions map[string]interface{} `json:"provider_options"`
	PromptPins      map[string]string      `json:"prompt_pins"`
}

// PromptVersion is an immutable prompt template.
type PromptVersion struct {
	ID           string    `json:"id"`
	PromptID     string    `json:"prompt_id"`
	Number       int       `json:"number"`
	Engine       string    `json:"engine"`
	Messages     []Message `json:"messages"`
	TextTemplate string    `json:"text_template"`
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

// Snapshot is a decoded GET /snapshot document: everything live in one
// environment, resolved locally with no further network calls.
type Snapshot struct {
	SchemaVersion  int
	Project        string
	Environment    string
	UseCases       map[string]*UseCase
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

type rawSnapshot struct {
	SchemaVersion  *int                      `json:"schema_version"`
	Project        string                    `json:"project"`
	Environment    string                    `json:"environment"`
	UseCases       map[string]*UseCase       `json:"use_cases"`
	Deployments    map[string]*Deployment    `json:"deployments"`
	PromptVersions map[string]*PromptVersion `json:"prompt_versions"`
	Models         map[string]*Model         `json:"models"`
}

// UnsupportedSchemaError is returned for a v1 or v2 snapshot: those documents
// describe routers, and this SDK only understands pins.
type UnsupportedSchemaError struct {
	Version int
}

func (e *UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("prompton: unsupported snapshot schema_version %d (this SDK reads v%d)", e.Version, SchemaVersion)
}

// ParseSnapshot decodes a GET /snapshot body.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	var raw rawSnapshot
	if err := decodeJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("prompton: invalid snapshot JSON: %w", err)
	}
	version := SchemaVersion
	var warnings []string
	if raw.SchemaVersion != nil {
		version = *raw.SchemaVersion
	}
	if version < SchemaVersion {
		return nil, &UnsupportedSchemaError{Version: version}
	}
	if version > SchemaVersion {
		warnings = append(warnings, fmt.Sprintf("unknown schema_version %d: decoding the fields this SDK knows", version))
	}
	if raw.UseCases == nil {
		return nil, fmt.Errorf("prompton: snapshot has no use_cases object")
	}

	snap := &Snapshot{
		SchemaVersion:  version,
		Project:        raw.Project,
		Environment:    raw.Environment,
		UseCases:       map[string]*UseCase{},
		Deployments:    map[string]*Deployment{},
		PromptVersions: map[string]*PromptVersion{},
		Models:         map[string]*Model{},
		Raw:            append([]byte(nil), data...),
		Warnings:       warnings,
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
func (s *Snapshot) PromptNames(useCase string) []string {
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
