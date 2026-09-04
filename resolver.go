package prompton

import (
	"github.com/polimo-dev/prompton-go/internal/liquid"
)

// DefaultPrompt is the prompt name used when a call names none.
const DefaultPrompt = "default"

// Source records where the configuration behind a use-case selection came from. It is
// sent with every monitoring log so a stale deployment is visible in the data.
type Source string

// The four use-case document sources.
const (
	SourceRemote Source = "remote"
	SourceDisk   Source = "disk"
	SourceBundle Source = "bundle"
	SourceManual Source = "manual"
)

// useCaseResolution is the answer to "what should this call use": the model to call,
// the parameters to send it, and the prompt to send — rendered when the call
// supplied variables, raw when it did not.
//
// The app calls the provider itself with Model, Params, ProviderOptions and
// Messages (or Text), then logs the call with the evidence carried
// here: DeploymentID, DeploymentRevision, Prompt and PromptVersionID.
type useCaseResolution struct {
	UseCase string
	Kind    Kind

	DeploymentID       string
	DeploymentRevision int

	// Prompt is the chosen prompt name, empty for an embedding use case.
	Prompt string
	// PromptNames lists every prompt name the live revision pins, sorted.
	PromptNames []string

	// Model is the provider-side model string to send; ModelID is the PromptOn
	// catalog id.
	Model    string
	ModelID  string
	Provider string

	// Params is use_case.default_params merged with deployment.params, and
	// ProviderOptions is model.provider_options merged with
	// deployment.provider_options. Both are shallow merges, right side wins.
	Params          map[string]interface{}
	ProviderOptions map[string]interface{}

	PromptVersionID     string
	PromptVersionNumber int
	Engine              string

	// Messages is set for a chat use case, Text for a text use case; an
	// embedding use case has neither.
	Messages []Message
	Text     string

	// Rendered reports whether Messages/Text went through the template engine.
	// Without variables the raw templates come back unrendered, which is what
	// prompt endpoint does too.
	Rendered bool

	InputSchema   []InputVariable
	PayloadPolicy *PayloadPolicy

	Source Source
	ETag   string

	// Warnings names ids the snapshot referenced but did not contain. A healthy
	// server never emits such a document.
	Warnings []string
}

// UseCaseOptions are the knobs of a resolve call.
type UseCaseOptions struct {
	// Prompt selects the prompt name; empty means "default". It is ignored for
	// an embedding use case rather than rejected.
	Prompt string
	// Variables renders the prompt. Nil leaves the raw template in place.
	Variables map[string]interface{}
	// Environment overrides the environment for a remote resolve. Local
	// resolution reads one document and refuses to answer for another
	// environment rather than silently serving the wrong pin.
	Environment string
}

// UseCaseOption configures a resolve call.
type UseCaseOption func(*UseCaseOptions)

// WithPrompt selects a prompt name.
func WithPrompt(name string) UseCaseOption {
	return func(o *UseCaseOptions) { o.Prompt = name }
}

// WithVariables renders the prompt with these variables.
func WithVariables(vars map[string]interface{}) UseCaseOption {
	return func(o *UseCaseOptions) { o.Variables = vars }
}

// WithEnvironment overrides the environment for this call. Only RemoteUseCase
// can honour it: a local Resolve fails with an "environment_mismatch"
// UseCaseError when it names anything other than the document in memory, since
// one process holds one environment's document.
func WithEnvironment(env string) UseCaseOption {
	return func(o *UseCaseOptions) { o.Environment = env }
}

// environmentMismatch is the error a local resolve returns when the call asked
// for an environment other than the one loaded.
func environmentMismatch(useCase, asked, loaded string) error {
	return &UseCaseError{
		Code:                "environment_mismatch",
		UseCase:             useCase,
		Environment:         asked,
		DocumentEnvironment: loaded,
	}
}

func buildResolveOptions(opts []UseCaseOption) UseCaseOptions {
	var o UseCaseOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}

// Resolve runs the resolution algorithm against a snapshot. It is a pure
// function: the same document and the same arguments always give the same
// answer, on every SDK.
//
//	deployment       = snapshot.deployments[use_case]
//	version          = snapshot.prompt_versions[deployment.prompt_pins[prompt]]
//	model            = snapshot.models[deployment.model_id]
//	params           = use_case.default_params <- deployment.params
//	provider_options = model.provider_options   <- deployment.provider_options
func resolveSnapshot(snap *UseCaseDocument, useCase string, opts ...UseCaseOption) (*useCaseResolution, error) {
	if snap == nil {
		return nil, ErrNotReady
	}
	o := buildResolveOptions(opts)
	if o.Environment != "" && o.Environment != snap.Environment {
		return nil, environmentMismatch(useCase, o.Environment, snap.Environment)
	}

	uc, ok := snap.UseCases[useCase]
	if !ok {
		return nil, &UseCaseError{Code: "unknown_use_case", UseCase: useCase}
	}
	dep, ok := snap.Deployments[useCase]
	if !ok || dep == nil {
		return nil, &UseCaseError{Code: "unresolved", UseCase: useCase}
	}

	res := &useCaseResolution{
		UseCase:            useCase,
		Kind:               uc.Kind,
		DeploymentID:       dep.ID,
		DeploymentRevision: dep.Revision,
		PromptNames:        snap.PromptNames(useCase),
		InputSchema:        uc.InputSchema,
		PayloadPolicy:      uc.PayloadPolicy,
		Source:             SourceManual,
		Params:             mergeParams(uc.DefaultParams, dep.Params),
	}

	var version *PromptVersion
	if uc.Kind != KindEmbedding {
		name := o.Prompt
		if name == "" {
			name = DefaultPrompt
		}
		versionID, pinned := dep.PromptPins[name]
		if !pinned {
			return nil, &UseCaseError{
				Code:        "unknown_prompt",
				UseCase:     useCase,
				Prompt:      name,
				PromptNames: res.PromptNames,
			}
		}
		res.Prompt = name
		if v, ok := snap.PromptVersions[versionID]; ok {
			version = v
		} else {
			res.Warnings = append(res.Warnings, "missing_prompt_version: "+versionID)
		}
	} else {
		// An embedding use case has no prompt at all; a name passed with the
		// request is ignored rather than rejected.
		res.PromptNames = []string{}
	}

	var model *Model
	if dep.ModelID != "" {
		if m, ok := snap.Models[dep.ModelID]; ok {
			model = m
		} else {
			res.Warnings = append(res.Warnings, "missing_model: "+dep.ModelID)
		}
	}

	if version != nil {
		res.PromptVersionID = version.ID
		res.PromptVersionNumber = version.Number
		res.Engine = version.Engine
		switch uc.Kind {
		case KindChat:
			res.Messages = append([]Message(nil), version.Messages...)
		case KindText:
			res.Text = version.TextTemplate
		}
	}
	if model != nil {
		res.ModelID = model.ID
		res.Model = model.ModelID
		res.Provider = model.Provider
		res.ProviderOptions = mergeParams(model.ProviderOptions, dep.ProviderOptions)
	} else {
		res.ProviderOptions = mergeParams(nil, dep.ProviderOptions)
	}

	if o.Variables != nil {
		if err := renderInto(res, o.Variables); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// Render renders a resolution's prompt with these variables and returns a copy
// carrying the rendered messages or text. The original is left untouched, so a
// resolution can be rendered once per call.
func (r *useCaseResolution) Render(vars map[string]interface{}) (*useCaseResolution, error) {
	clone := *r
	clone.Messages = append([]Message(nil), r.Messages...)
	if err := renderInto(&clone, vars); err != nil {
		return nil, err
	}
	return &clone, nil
}

// RenderMessages renders a chat resolution and returns just the messages.
func (r *useCaseResolution) RenderMessages(vars map[string]interface{}) ([]Message, error) {
	if r.Kind != KindChat || r.Messages == nil {
		return nil, ErrNoTemplate
	}
	rendered, err := r.Render(vars)
	if err != nil {
		return nil, err
	}
	return rendered.Messages, nil
}

// RenderText renders a text resolution and returns just the text.
func (r *useCaseResolution) RenderText(vars map[string]interface{}) (string, error) {
	if r.Kind != KindText || r.Text == "" {
		return "", ErrNoTemplate
	}
	rendered, err := r.Render(vars)
	if err != nil {
		return "", err
	}
	return rendered.Text, nil
}

func renderInto(res *useCaseResolution, vars map[string]interface{}) error {
	engine := liquid.Engine(res.Engine)
	if engine != liquid.EngineRaw {
		engine = liquid.EngineLiquid
	}
	switch res.Kind {
	case KindChat:
		if res.Messages == nil {
			return nil
		}
		out := make([]Message, len(res.Messages))
		for i, m := range res.Messages {
			content, err := liquid.Render(m.Content, vars, engine)
			if err != nil {
				return templateError(err)
			}
			m.Content = content
			out[i] = m
		}
		res.Messages = out
		res.Rendered = true
	case KindText:
		if res.Text == "" {
			return nil
		}
		text, err := liquid.Render(res.Text, vars, engine)
		if err != nil {
			return templateError(err)
		}
		res.Text = text
		res.Rendered = true
	}
	return nil
}

func templateError(err *liquid.Error) error {
	if err == nil {
		return nil
	}
	if err.Category == liquid.CategoryMissing {
		return &MissingVariableError{Variable: err.Variable}
	}
	return &TemplateError{Category: err.Category, Message: err.Message}
}

// RenderTemplate renders one template source with variables. It is the same
// engine the resolver uses, exposed for tooling and tests.
func RenderTemplate(source string, vars map[string]interface{}, engine string) (string, error) {
	e := liquid.Engine(engine)
	if e != liquid.EngineRaw {
		e = liquid.EngineLiquid
	}
	out, err := liquid.Render(source, vars, e)
	if err != nil {
		return "", templateError(err)
	}
	return out, nil
}

// LintReason is one way a template falls outside the subset PromptOn allows.
// Kind is "whitespace_control", "disallowed_tag", "disallowed_filter" or
// "parse"; Value names the offending marker, tag, filter or parser message.
type LintReason struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// LintTemplate checks a template against the whitelist PromptOn enforces when a
// prompt version is committed: the six tags, the three filters, and no
// whitespace control. It returns nil when the template is acceptable.
func LintTemplate(source string) []LintReason {
	reasons := liquid.Lint(source)
	if len(reasons) == 0 {
		return nil
	}
	out := make([]LintReason, len(reasons))
	for i, r := range reasons {
		out[i] = LintReason{Kind: r.Kind, Value: r.Value}
	}
	return out
}

// TemplateVariables lists the top-level input variables a template reads.
func TemplateVariables(source string) []string {
	return liquid.Variables(source)
}
