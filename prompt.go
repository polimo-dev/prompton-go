package prompton

import "context"

// Prompt is the resolved PromptOn configuration for one application prompt.
// It carries the model, provider options, selected prompt, and log evidence for
// a provider call the application performs itself.
type Prompt struct {
	promptResolution *promptResolution
	Key              string
	Kind             Kind
	Model            string
	ModelID          string
	Provider         string
	Params           map[string]interface{}
	Tools            map[string]interface{}
	Decision         map[string]interface{}

	ProviderOptions     map[string]interface{}
	Variables           []Variable
	DeploymentID        string
	DeploymentRevision  string
	Template            string
	TemplateNames       []string
	PromptVersionID     string
	PromptVersionNumber int
	Source              Source

	client *Client
}

// Prompt answers "what should this call use", from the snapshot in memory.
// There is no network call on this path: the SDK serves the document it has
// and refreshes stale snapshots in the background.
func (c *Client) Prompt(ctx context.Context, key string, opts ...PromptOption) (*Prompt, error) {
	res, err := c.resolve(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	return newPrompt(c, res), nil
}

func newPrompt(c *Client, res *promptResolution) *Prompt {
	u := &Prompt{client: c}
	u.replaceResolution(res)
	return u
}

// Messages renders this chat prompt. Use WithTemplate to select another pinned
// template name; the returned messages and subsequent Track evidence use that
// selected prompt.
func (u *Prompt) Messages(ctx context.Context, vars map[string]interface{}, opts ...PromptOption) ([]Message, error) {
	res, err := u.selectResolution(ctx, opts...)
	if err != nil {
		return nil, err
	}
	u.replaceResolution(res)
	return res.RenderMessages(vars)
}

// Text renders this text prompt. Use WithTemplate to select another pinned
// template name; the returned text and subsequent Track evidence use that
// selected prompt.
func (u *Prompt) Text(ctx context.Context, vars map[string]interface{}, opts ...PromptOption) (string, error) {
	res, err := u.selectResolution(ctx, opts...)
	if err != nil {
		return "", err
	}
	u.replaceResolution(res)
	return res.RenderText(vars)
}

// Decision renders this decision prompt and returns the native decision payload.
func (u *Prompt) DecisionPayload(ctx context.Context, vars map[string]interface{}, opts ...PromptOption) (map[string]interface{}, error) {
	res, err := u.selectResolution(ctx, opts...)
	if err != nil {
		return nil, err
	}
	u.replaceResolution(res)
	return res.RenderDecision(vars)
}

func (u *Prompt) replaceResolution(res *promptResolution) {
	u.promptResolution = res
	u.Key = res.Prompt
	u.Kind = res.Kind
	u.Model = res.Model
	u.ModelID = res.ModelID
	u.Provider = res.Provider
	u.Params = res.Params
	u.Tools = res.Tools
	u.Decision = res.Decision
	u.ProviderOptions = res.ProviderOptions
	u.Variables = res.Variables
	u.DeploymentID = res.DeploymentID
	u.DeploymentRevision = res.DeploymentRevision
	u.Template = res.Template
	u.TemplateNames = res.TemplateNames
	u.PromptVersionID = res.PromptVersionID
	u.PromptVersionNumber = res.PromptVersionNumber
	u.Source = res.Source
}

// Track times a provider call, logs it, and returns the provider result
// unchanged.
func (u *Prompt) Track(
	ctx context.Context,
	meta CallMeta,
	fn func(context.Context) (*Result, error),
) (*Result, error) {
	if u == nil || u.client == nil {
		return nil, ErrNotReady
	}
	return u.client.trackResolved(ctx, u.promptResolution, meta, fn)
}

func (u *Prompt) selectResolution(ctx context.Context, opts ...PromptOption) (*promptResolution, error) {
	if u == nil || u.promptResolution == nil {
		return nil, ErrNotReady
	}
	o := buildResolveOptions(opts)
	if o.Template == "" && o.Environment == "" {
		return u.promptResolution, nil
	}
	if u.client == nil {
		if o.Environment != "" || (o.Template != "" && o.Template != u.promptResolution.Prompt) {
			return nil, ErrNotReady
		}
		return u.promptResolution, nil
	}
	return u.client.resolve(ctx, u.promptResolution.Prompt, opts...)
}
