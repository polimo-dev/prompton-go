package prompton

import "context"

// UseCase is the resolved PromptOn configuration for one application use case.
// It carries the model, provider options, selected prompt, and log evidence for
// a provider call the application performs itself.
type UseCase struct {
	useCaseResolution *useCaseResolution
	Key               string
	Kind              Kind
	Model             string
	ModelID           string
	Provider          string
	Params            map[string]interface{}

	ProviderOptions     map[string]interface{}
	DeploymentID        string
	DeploymentRevision  int
	Prompt              string
	PromptNames         []string
	PromptVersionID     string
	PromptVersionNumber int
	Source              Source

	client *Client
}

// UseCase answers "what should this call use", from the snapshot in memory.
// There is no network call on this path: the SDK serves the document it has
// and refreshes stale snapshots in the background.
func (c *Client) UseCase(ctx context.Context, key string, opts ...UseCaseOption) (*UseCase, error) {
	res, err := c.resolve(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	return newUseCase(c, res), nil
}

func newUseCase(c *Client, res *useCaseResolution) *UseCase {
	u := &UseCase{client: c}
	u.replaceResolution(res)
	return u
}

// Messages renders this chat use case. Use WithPrompt to select another pinned
// prompt name; the returned messages and subsequent Track evidence use that
// selected prompt.
func (u *UseCase) Messages(ctx context.Context, vars map[string]interface{}, opts ...UseCaseOption) ([]Message, error) {
	res, err := u.selectResolution(ctx, opts...)
	if err != nil {
		return nil, err
	}
	u.replaceResolution(res)
	return res.RenderMessages(vars)
}

// Text renders this text use case. Use WithPrompt to select another pinned
// prompt name; the returned text and subsequent Track evidence use that
// selected prompt.
func (u *UseCase) Text(ctx context.Context, vars map[string]interface{}, opts ...UseCaseOption) (string, error) {
	res, err := u.selectResolution(ctx, opts...)
	if err != nil {
		return "", err
	}
	u.replaceResolution(res)
	return res.RenderText(vars)
}

func (u *UseCase) replaceResolution(res *useCaseResolution) {
	u.useCaseResolution = res
	u.Key = res.UseCase
	u.Kind = res.Kind
	u.Model = res.Model
	u.ModelID = res.ModelID
	u.Provider = res.Provider
	u.Params = res.Params
	u.ProviderOptions = res.ProviderOptions
	u.DeploymentID = res.DeploymentID
	u.DeploymentRevision = res.DeploymentRevision
	u.Prompt = res.Prompt
	u.PromptNames = res.PromptNames
	u.PromptVersionID = res.PromptVersionID
	u.PromptVersionNumber = res.PromptVersionNumber
	u.Source = res.Source
}

// Track times a provider call, logs it, and returns the provider result
// unchanged.
func (u *UseCase) Track(
	ctx context.Context,
	meta CallMeta,
	fn func(context.Context) (*Result, error),
) (*Result, error) {
	if u == nil || u.client == nil {
		return nil, ErrNotReady
	}
	return u.client.trackResolved(ctx, u.useCaseResolution, meta, fn)
}

func (u *UseCase) selectResolution(ctx context.Context, opts ...UseCaseOption) (*useCaseResolution, error) {
	if u == nil || u.useCaseResolution == nil {
		return nil, ErrNotReady
	}
	o := buildResolveOptions(opts)
	if o.Prompt == "" && o.Environment == "" {
		return u.useCaseResolution, nil
	}
	if u.client == nil {
		if o.Environment != "" || (o.Prompt != "" && o.Prompt != u.useCaseResolution.Prompt) {
			return nil, ErrNotReady
		}
		return u.useCaseResolution, nil
	}
	return u.client.resolve(ctx, u.useCaseResolution.UseCase, opts...)
}
