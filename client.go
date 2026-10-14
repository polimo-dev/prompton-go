package prompton

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Client is the SDK. It holds the snapshot your app resolves against and the
// buffer your monitoring logs leave through.
//
// PromptOn is a control plane, not a proxy: the client never calls your model
// provider and never sees your provider key. If PromptOn is down the client
// keeps answering from the last document it received, so a generation never
// fails because PromptOn did — the configuration is stale in the worst case,
// not absent.
//
// A Client is safe for concurrent use. Close it on shutdown to flush what is
// still queued.
type Client struct {
	cfg    Config
	store  *snapshotStore
	buffer *logBuffer

	fetchMu  sync.Mutex
	inflight map[string]*promptFetch

	pollCh    chan struct{}
	done      chan struct{}
	pollDone  chan struct{}
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool

	warnMu sync.Mutex
	warned map[string]time.Time

	recordedMu      sync.Mutex
	recorded        []map[string]interface{}
	recordedDropped int
	recordedClosed  int

	resolveMu    sync.Mutex
	resolveCache map[string]*cachedResolve
}

type promptFetch struct {
	done     chan struct{}
	err      error
	started  time.Time
	deadline time.Time
}

type cachedResolve struct {
	response *resolveResponse
	at       time.Time
}

const configFetchBudget = time.Second

// New builds a client and loads whatever configuration is already on hand:
// memory, then the disk cache, then the bundle. New never contacts PromptOn;
// live configuration is fetched on demand for the specific prompt being
// resolved.
func New(cfg Config) (*Client, error) {
	resolved, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:          resolved,
		store:        &snapshotStore{entries: map[string]*snapshotEntry{}},
		inflight:     map[string]*promptFetch{},
		pollCh:       make(chan struct{}, 1),
		done:         make(chan struct{}),
		pollDone:     make(chan struct{}),
		warned:       map[string]time.Time{},
		resolveCache: map[string]*cachedResolve{},
	}

	c.loadLocal()

	if c.cfg.Mode == ModeLive {
		if c.cfg.APIKey == "" {
			c.cfg.Logger("no API key configured (PTN_API_KEY): resolving from %s only, and monitoring logs are kept in memory", c.localTierDescription())
		} else {
			c.buffer = newLogBuffer(c)
			close(c.pollDone)
			return c, nil
		}
	}
	close(c.pollDone)
	return c, nil
}

// loadLocal fills the store from the disk cache, then the bundle. A document
// for another environment or project is never used.
func (c *Client) loadLocal() {
	if c.cfg.BundlePath != "" {
		if entry, err := readSnapshotFile(c.cfg.BundlePath, c.cfg.Environment, c.cfg.Project); err == nil {
			entry.source = SourceBundle
			entry.fetchedAt = c.cfg.now()
			c.store.putDocument(entry)
		} else if !os.IsNotExist(err) {
			c.cfg.Logger("ignoring the snapshot bundle at %s: %v", c.cfg.BundlePath, err)
		}
	}
	if !c.cfg.DisableDiskCache && c.cfg.DiskCachePath != "" {
		if entry, err := readSnapshotFile(c.cfg.DiskCachePath, c.cfg.Environment, c.cfg.Project); err == nil {
			entry.source = SourceDisk
			if entry.fetchedAt.IsZero() {
				entry.fetchedAt = c.cfg.now()
			}
			c.store.putDocument(entry)
		} else if !os.IsNotExist(err) {
			// A corrupt or partial file is ignored, not an error: another
			// process may be renaming a new one into place right now.
			c.cfg.Logger("ignoring the snapshot disk cache at %s: %v", c.cfg.DiskCachePath, err)
		}
		if entries, err := readKeySnapshotFiles(c.cfg.DiskCachePath, c.cfg.Environment, c.cfg.Project); err == nil {
			for _, entry := range entries {
				entry.source = SourceDisk
				if entry.fetchedAt.IsZero() {
					entry.fetchedAt = c.cfg.now()
				}
				for key := range entry.snapshot.Prompts {
					c.store.put(key, entry)
				}
			}
		} else if !os.IsNotExist(err) {
			c.cfg.Logger("ignoring the prompt disk cache at %s: %v", keySnapshotDir(c.cfg.DiskCachePath), err)
		}
	}
}

func (c *Client) localTierDescription() string {
	if entry := c.store.any(); entry != nil {
		return string(entry.source)
	}
	if c.cfg.BundlePath != "" || c.cfg.DiskCachePath != "" {
		return "the disk cache and bundle"
	}
	return "nothing"
}

// Close flushes what is queued, best effort. It is safe to call more than
// once.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
		<-c.pollDone
		if c.buffer != nil {
			c.closeErr = c.buffer.close(c.cfg.ShutdownTimeout)
		}
	})
	return c.closeErr
}

// Environment is the environment this client reads.
func (c *Client) Environment() string { return c.cfg.Environment }

// Project is the project slug this client reads.
func (c *Client) Project() string { return c.cfg.Project }

func (c *Client) warnOnce(key, format string, args ...interface{}) {
	now := c.cfg.now()
	c.warnMu.Lock()
	last, seen := c.warned[key]
	if seen && now.Sub(last) < time.Minute {
		c.warnMu.Unlock()
		return
	}
	c.warned[key] = now
	c.warnMu.Unlock()
	c.cfg.Logger(format, args...)
}

// ---------------------------------------------------------------------------
// resolution

// Resolve answers "what should this call use" from the cached document for
// this prompt. In live mode it first gives that key one bounded chance to
// refresh when the cache is missing or stale; failures fall back to the last
// cached value.
//
// Pass WithVariables to get the prompt rendered; without it the raw templates
// come back, which is also what prompt endpoint does.
//
// WithEnvironment is refused here rather than ignored: this client holds one
// environment's document, and answering a staging call from the production pin
// is precisely the accident the environment guard exists to prevent. Use
// RenderPrompt, or a second client, for another environment.
func (c *Client) resolve(ctx context.Context, prompt string, opts ...PromptOption) (*promptResolution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env := buildResolveOptions(opts).Environment; env != "" && env != c.cfg.Environment {
		return nil, environmentMismatch(prompt, env, c.cfg.Environment)
	}
	if c.cfg.Mode == ModeLive && c.cfg.APIKey != "" {
		_ = c.ensurePromptFresh(ctx, prompt)
	}
	entry := c.store.get(prompt)
	if entry == nil {
		// Nothing anywhere: this is the one case where resolution fails.
		return nil, ErrNotReady
	}
	res, err := resolveSnapshot(entry.snapshot, prompt, opts...)
	if err != nil {
		return nil, err
	}
	res.Source = entry.source
	res.ETag = entry.etag
	return res, nil
}

// TemplateNames lists the template names the live revision of a prompt pins. It
// is exactly the set of values WithTemplate accepts.
func (c *Client) TemplateNames(prompt string) ([]string, error) {
	if c.cfg.Mode == ModeLive && c.cfg.APIKey != "" {
		_ = c.ensurePromptFresh(context.Background(), prompt)
	}
	entry := c.store.get(prompt)
	if entry == nil {
		return nil, ErrNotReady
	}
	if _, ok := entry.snapshot.Prompts[prompt]; !ok {
		return nil, &PromptError{Code: "unknown_template", Prompt: prompt}
	}
	return entry.snapshot.TemplateNames(prompt), nil
}

func (c *Client) isStale(entry *snapshotEntry) bool {
	if entry.source != SourceRemote {
		return true
	}
	return c.cfg.now().Sub(entry.fetchedAt) >= c.cfg.configCacheTTL
}

func (c *Client) ensurePromptFresh(ctx context.Context, key string) error {
	now := c.cfg.now()
	if entry := c.store.get(key); entry != nil && !c.isStale(entry) {
		return nil
	}
	c.fetchMu.Lock()
	if in := c.inflight[key]; in != nil {
		c.fetchMu.Unlock()
		return c.waitForPromptFetch(ctx, in)
	}
	if last := c.store.lastAttempt(key); !last.IsZero() && now.Sub(last) < c.cfg.configCacheTTL {
		c.fetchMu.Unlock()
		return nil
	}
	timeout := c.cfg.Timeout
	if timeout > configFetchBudget {
		timeout = configFetchBudget
	}
	in := &promptFetch{
		done:     make(chan struct{}),
		started:  now,
		deadline: time.Now().Add(timeout),
	}
	c.inflight[key] = in
	c.store.noteAttempt(key, now)
	c.fetchMu.Unlock()

	go func() {
		in.err = c.fetchPrompt(key, in.started, in.deadline, timeout)
		close(in.done)
		c.fetchMu.Lock()
		if c.inflight[key] == in {
			delete(c.inflight, key)
		}
		c.fetchMu.Unlock()
	}()

	return c.waitForPromptFetch(ctx, in)
}

func (c *Client) waitForPromptFetch(ctx context.Context, in *promptFetch) error {
	timer := time.NewTimer(time.Until(in.deadline))
	defer timer.Stop()
	select {
	case <-in.done:
		return in.err
	case <-timer.C:
		return context.DeadlineExceeded
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) fetchPrompt(key string, started, deadline time.Time, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	etag := ""
	if entry := c.store.get(key); entry != nil {
		etag = entry.etag
	}
	resp, err := c.fetchPromptSnapshot(reqCtx, key, c.cfg.Environment, etag)
	now := c.cfg.now()
	if err != nil {
		c.store.markStale(key, now)
		c.warnOnce("prompt-fetch:"+key, "config fetch for %s failed (%v); serving the cached document if present", key, err)
		return err
	}

	switch resp.Status {
	case 304:
		if reqCtx.Err() != nil || time.Now().After(deadline) {
			c.store.markStale(key, now)
			return context.DeadlineExceeded
		}
		if c.store.get(key) == nil {
			return fmt.Errorf("prompton: config fetch for %s returned 304 without a cached document", key)
		}
		c.store.markFresh(key, SourceRemote, now)
		return nil
	case 200:
		snap, parseErr := ParsePromptDocument(resp.Body)
		if parseErr != nil {
			c.store.markStale(key, now)
			return parseErr
		}
		if guardErr := guardDocument(snap, c.cfg.Environment, c.cfg.Project); guardErr != nil {
			c.store.markStale(key, now)
			return guardErr
		}
		if _, ok := snap.Prompts[key]; !ok {
			c.store.markStale(key, now)
			return fmt.Errorf("prompton: config fetch for %s returned a document without that prompt", key)
		}
		if reqCtx.Err() != nil || time.Now().After(deadline) {
			c.store.markStale(key, now)
			return context.DeadlineExceeded
		}
		for _, w := range snap.Warnings {
			c.warnOnce("snapshot-warning:"+w, "snapshot: %s", w)
		}
		c.store.put(key, &snapshotEntry{
			snapshot:     snap,
			etag:         resp.ETag,
			lastModified: resp.LastModified,
			source:       SourceRemote,
			fetchedAt:    now,
			lastAttempt:  started,
		})
		c.persistKeyDisk(key, snap, resp, now)
		return nil
	default:
		c.store.markStale(key, now)
		return fmt.Errorf("prompton: unexpected config fetch status %d", resp.Status)
	}
}

// PromptDocumentInfo reports which document resolution is reading and how old it is.
func (c *Client) PromptDocumentInfo() PromptDocumentInfo {
	entry := c.store.any()
	if entry == nil {
		return PromptDocumentInfo{}
	}
	return PromptDocumentInfo{
		Source:       entry.source,
		Project:      entry.snapshot.Project,
		Environment:  entry.snapshot.Environment,
		ETag:         entry.etag,
		LastModified: entry.lastModified,
		FetchedAt:    entry.fetchedAt,
		Stale:        entry.source != SourceRemote || !entry.staleSince.IsZero(),
		Age:          c.cfg.now().Sub(entry.fetchedAt),
		Loaded:       true,
	}
}

// PromptDocument returns the document resolution is currently reading, or nil.
func (c *Client) PromptDocument() *PromptDocument {
	entry := c.store.any()
	if entry == nil {
		return nil
	}
	return entry.snapshot
}

// SetPromptDocument installs a document by hand, reported as source
// "manual". It is how test mode is seeded, and how a script pins a known
// configuration.
func (c *Client) SetPromptDocument(data []byte) error {
	snap, err := ParsePromptDocument(data)
	if err != nil {
		return err
	}
	c.store.putDocument(&snapshotEntry{snapshot: snap, source: SourceManual, fetchedAt: c.cfg.now()})
	return nil
}

// SetPromptDocumentFile installs a document from a file.
func (c *Client) SetPromptDocumentFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.SetPromptDocument(data)
}

// ExportPromptDocument writes the current document and its sidecar to path, which is
// how a bundle is built: run it in CI and commit the result so a cold start
// with no disk cache and no network still resolves.
func (c *Client) ExportPromptDocument(path string) error {
	entry := c.store.any()
	if entry == nil {
		return ErrNotReady
	}
	return writeSnapshotFile(path, entry.snapshot.Raw, sidecar{
		ETag:         entry.etag,
		LastModified: entry.lastModified,
		Environment:  entry.snapshot.Environment,
		Project:      entry.snapshot.Project,
		FetchedAt:    c.cfg.now().UTC().Format(time.RFC3339Nano),
	})
}

// ---------------------------------------------------------------------------
// refresh

// Refresh is a compatibility no-op. Runtime lookup refreshes only the requested
// key through Prompt/TemplateNames, and bundle tooling should use ExportPromptDocument
// with the document already cached by those key lookups.
func (c *Client) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.cfg.Mode == ModeOffline {
		c.loadLocal()
	}
	// Runtime refresh is demand-driven through Prompt/TemplateNames so a no-arg
	// live refresh must not bulk-fetch all prompts. ExportPromptDocument writes
	// the currently cached document for explicit bundle tooling.
	return nil
}

func (c *Client) persistKeyDisk(key string, snap *PromptDocument, resp *snapshotResponse, fetchedAt time.Time) {
	if c.cfg.DisableDiskCache || c.cfg.DiskCachePath == "" {
		return
	}
	path := keySnapshotPath(c.cfg.DiskCachePath, key)
	err := writeSnapshotFile(path, snap.Raw, sidecar{
		ETag:         resp.ETag,
		LastModified: resp.LastModified,
		Environment:  snap.Environment,
		Project:      snap.Project,
		FetchedAt:    fetchedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		c.warnOnce("prompt-disk-write", "could not write the prompt disk cache at %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// prompt endpoint

// RenderPrompt asks the server to resolve, which is the simple path and the
// smoke test for a deployment. It is not for a hot loop: the answer is cached
// for the same TTL as the snapshot, per prompt, prompt and environment, and
// variables are rendered locally against that cached template. When the server
// rate limits, fails or is unreachable, the cached answer is served.
func (c *Client) RenderPrompt(ctx context.Context, prompt string, opts ...PromptOption) (*Prompt, error) {
	o := buildResolveOptions(opts)
	env := o.Environment
	if env == "" {
		env = c.cfg.Environment
	}
	template := o.Template
	key := env + "\x00" + prompt + "\x00" + template

	c.resolveMu.Lock()
	cached, hasCached := c.resolveCache[key]
	fresh := hasCached && c.cfg.now().Sub(cached.at) < c.cfg.CacheTTL
	c.resolveMu.Unlock()

	var response *resolveResponse
	switch {
	case fresh:
		response = cached.response
	default:
		fetched, err := c.postResolve(ctx, resolveRequest{
			Prompt:      prompt,
			Environment: env,
			Template:    template,
		})
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
				return nil, promptErrorFromAPI(prompt, apiErr)
			}
			if hasCached {
				c.warnOnce("resolve-remote", "prompt endpoint failed (%v); serving the cached answer", err)
				response = cached.response
			} else {
				return nil, err
			}
		} else {
			response = fetched
			c.resolveMu.Lock()
			c.resolveCache[key] = &cachedResolve{response: fetched, at: c.cfg.now()}
			c.resolveMu.Unlock()
		}
	}

	res := resolutionFromResponse(prompt, response)
	if o.Variables != nil {
		if err := renderInto(res, o.Variables); err != nil {
			return nil, err
		}
	}
	return newPrompt(c, res), nil
}

func resolutionFromResponse(prompt string, r *resolveResponse) *promptResolution {
	res := &promptResolution{
		Prompt:             prompt,
		Kind:               Kind(r.Kind),
		DeploymentID:       r.Deployment.ID,
		DeploymentRevision: r.Deployment.Revision,
		TemplateNames:      r.TemplateNames,
		Params:             r.Params,
		Tools:              r.Tools,
		ProviderOptions:    r.ProviderOptions,
		Messages:           append([]Message(nil), r.Messages...),
		Source:             SourceRemote,
		ETag:               r.ETag,
		Warnings:           r.Warnings,
	}
	if r.Source != "" {
		res.Source = Source(r.Source)
	}
	if res.TemplateNames == nil {
		res.TemplateNames = []string{}
	}
	if r.Template != nil {
		res.Template = *r.Template
	}
	if r.Model != nil {
		res.Model = *r.Model
	}
	if r.ModelID != nil {
		res.ModelID = *r.ModelID
	}
	if r.Provider != nil {
		res.Provider = *r.Provider
	}
	var err error
	res.Params, err = mergeToolParams(res.Params, res.Tools)
	if err != nil {
		res.Warnings = append(res.Warnings, err.Error())
	}
	if r.PromptVersion != nil {
		res.PromptVersionID = r.PromptVersion.ID
		res.PromptVersionNumber = r.PromptVersion.Number
	}
	if r.Text != nil {
		res.Text = *r.Text
	}
	return res
}
