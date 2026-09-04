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

	refreshMu sync.Mutex
	stateMu   sync.Mutex
	notBefore time.Time
	failures  int

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

type cachedResolve struct {
	response *resolveResponse
	at       time.Time
}

// New builds a client and loads whatever configuration is already on hand:
// memory, then the disk cache, then the bundle. The first remote fetch happens
// in the background, so New never blocks on the network and the first
// generation never waits for it.
func New(cfg Config) (*Client, error) {
	resolved, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:          resolved,
		store:        &snapshotStore{},
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
			go c.pollLoop()
			return c, nil
		}
	}
	close(c.pollDone)
	return c, nil
}

// loadLocal fills the store from the disk cache, then the bundle. A document
// for another environment or project is never used.
func (c *Client) loadLocal() {
	if !c.cfg.DisableDiskCache && c.cfg.DiskCachePath != "" {
		if entry, err := readSnapshotFile(c.cfg.DiskCachePath, c.cfg.Environment, c.cfg.Project); err == nil {
			entry.source = SourceDisk
			if entry.fetchedAt.IsZero() {
				entry.fetchedAt = c.cfg.now()
			}
			c.store.put(entry)
			return
		} else if !os.IsNotExist(err) {
			// A corrupt or partial file is ignored, not an error: another
			// process may be renaming a new one into place right now.
			c.cfg.Logger("ignoring the snapshot disk cache at %s: %v", c.cfg.DiskCachePath, err)
		}
	}
	if c.cfg.BundlePath != "" {
		if entry, err := readSnapshotFile(c.cfg.BundlePath, c.cfg.Environment, c.cfg.Project); err == nil {
			entry.source = SourceBundle
			entry.fetchedAt = c.cfg.now()
			c.store.put(entry)
			return
		} else if !os.IsNotExist(err) {
			c.cfg.Logger("ignoring the snapshot bundle at %s: %v", c.cfg.BundlePath, err)
		}
	}
}

func (c *Client) localTierDescription() string {
	if entry := c.store.get(); entry != nil {
		return string(entry.source)
	}
	if c.cfg.BundlePath != "" || c.cfg.DiskCachePath != "" {
		return "the disk cache and bundle"
	}
	return "nothing"
}

// Close flushes what is queued, best effort, and stops the background refresh.
// It is safe to call more than once.
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

// Resolve answers "what should this call use", from the snapshot in memory.
// There is no network call on this path: within the cache TTL the document is
// served as it is, and past it a refresh runs in the background without
// blocking or failing this call.
//
// Pass WithVariables to get the prompt rendered; without it the raw templates
// come back, which is also what POST /resolve does.
//
// WithEnvironment is refused here rather than ignored: this client holds one
// environment's document, and answering a staging call from the production pin
// is precisely the accident the environment guard exists to prevent. Use
// ResolveRemote, or a second client, for another environment.
func (c *Client) Resolve(ctx context.Context, useCase string, opts ...ResolveOption) (*Resolution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env := buildResolveOptions(opts).Environment; env != "" && env != c.cfg.Environment {
		return nil, environmentMismatch(useCase, env, c.cfg.Environment)
	}
	entry := c.store.get()
	if entry == nil {
		// Nothing anywhere: this is the one case where resolution fails.
		return nil, ErrNotReady
	}
	if c.isStale(entry) {
		c.nudgeRefresh()
	}
	res, err := Resolve(entry.snapshot, useCase, opts...)
	if err != nil {
		return nil, err
	}
	res.Source = entry.source
	res.ETag = entry.etag
	return res, nil
}

// PromptNames lists the prompt names the live revision of a use case pins. It
// is exactly the set of values WithPrompt accepts.
func (c *Client) PromptNames(useCase string) ([]string, error) {
	entry := c.store.get()
	if entry == nil {
		return nil, ErrNotReady
	}
	if _, ok := entry.snapshot.UseCases[useCase]; !ok {
		return nil, &ResolveError{Code: "unknown_use_case", UseCase: useCase}
	}
	return entry.snapshot.PromptNames(useCase), nil
}

func (c *Client) isStale(entry *snapshotEntry) bool {
	if entry.source != SourceRemote {
		return true
	}
	return c.cfg.now().Sub(entry.fetchedAt) >= c.cfg.CacheTTL
}

func (c *Client) nudgeRefresh() {
	select {
	case c.pollCh <- struct{}{}:
	default:
	}
}

// SnapshotInfo reports which document resolution is reading and how old it is.
func (c *Client) SnapshotInfo() SnapshotInfo {
	entry := c.store.get()
	if entry == nil {
		return SnapshotInfo{}
	}
	return SnapshotInfo{
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

// Snapshot returns the document resolution is currently reading, or nil.
func (c *Client) Snapshot() *Snapshot {
	entry := c.store.get()
	if entry == nil {
		return nil
	}
	return entry.snapshot
}

// SetSnapshot installs a document by hand, reported as resolution_source
// "manual". It is how test mode is seeded, and how a script pins a known
// configuration.
func (c *Client) SetSnapshot(data []byte) error {
	snap, err := ParseSnapshot(data)
	if err != nil {
		return err
	}
	c.store.put(&snapshotEntry{snapshot: snap, source: SourceManual, fetchedAt: c.cfg.now()})
	return nil
}

// SetSnapshotFile installs a document from a file.
func (c *Client) SetSnapshotFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.SetSnapshot(data)
}

// ExportSnapshot writes the current document and its sidecar to path, which is
// how a bundle is built: run it in CI and commit the result so a cold start
// with no disk cache and no network still resolves.
func (c *Client) ExportSnapshot(path string) error {
	entry := c.store.get()
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

// Refresh fetches the snapshot now and waits for the result. Use it in scripts
// and one-shot jobs; long-running processes get the same thing from the
// background poll.
func (c *Client) Refresh(ctx context.Context) error {
	if c.cfg.Mode == ModeOffline {
		c.loadLocal()
		return nil
	}
	if c.cfg.Mode == ModeTest {
		return nil
	}
	if c.cfg.APIKey == "" {
		return ErrNoAPIKey
	}
	_, err := c.refreshOnce(ctx, true)
	return err
}

func (c *Client) pollLoop() {
	defer close(c.pollDone)
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-c.pollCh:
		case <-timer.C:
		}
		next, _ := c.refreshOnce(context.Background(), false)
		resetTimer(timer, next)
	}
}

// refreshOnce performs one conditional fetch and returns how long to wait
// before the next attempt. A refresh never blocks or fails a generation: while
// it is in flight, and if it fails, the previous document keeps being served.
func (c *Client) refreshOnce(ctx context.Context, force bool) (time.Duration, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	now := c.cfg.now()
	if !force {
		c.stateMu.Lock()
		wait := c.notBefore.Sub(now)
		c.stateMu.Unlock()
		if wait > 0 {
			return wait, nil
		}
	}

	etag := ""
	if entry := c.store.get(); entry != nil {
		etag = entry.etag
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	resp, err := c.fetchSnapshot(reqCtx, c.cfg.Environment, etag)
	cancel()

	if err != nil {
		return c.refreshFailed(err, resp), err
	}

	switch resp.Status {
	case 304:
		// The server confirmed the ETag is current, which also promotes a
		// document that came off disk or out of the bundle.
		c.store.markFresh(SourceRemote, c.cfg.now())
		c.refreshSucceeded()
		return c.cfg.CacheTTL, nil
	case 200:
		snap, parseErr := ParseSnapshot(resp.Body)
		if parseErr != nil {
			return c.refreshFailed(parseErr, resp), parseErr
		}
		if guardErr := guardDocument(snap, c.cfg.Environment, c.cfg.Project); guardErr != nil {
			return c.refreshFailed(guardErr, resp), guardErr
		}
		for _, w := range snap.Warnings {
			c.warnOnce("snapshot-warning:"+w, "snapshot: %s", w)
		}
		fetchedAt := c.cfg.now()
		c.store.put(&snapshotEntry{
			snapshot:     snap,
			etag:         resp.ETag,
			lastModified: resp.LastModified,
			source:       SourceRemote,
			fetchedAt:    fetchedAt,
		})
		c.persistDisk(snap, resp, fetchedAt)
		c.refreshSucceeded()
		return c.cfg.CacheTTL, nil
	default:
		unexpected := fmt.Errorf("prompton: unexpected snapshot status %d", resp.Status)
		return c.refreshFailed(unexpected, resp), unexpected
	}
}

func (c *Client) refreshSucceeded() {
	c.stateMu.Lock()
	c.failures = 0
	c.notBefore = time.Time{}
	c.stateMu.Unlock()
}

// refreshFailed keeps serving the previous document and decides when the SDK
// may talk to the server again. On 429 that is whatever Retry-After said;
// otherwise the TTL doubled per consecutive failure, up to five minutes.
func (c *Client) refreshFailed(err error, resp *snapshotResponse) time.Duration {
	now := c.cfg.now()
	c.store.markStale(now)

	var retryAfter time.Duration
	var apiErr *APIError
	rateLimited := false
	if errors.As(err, &apiErr) {
		if apiErr.Status == 429 {
			rateLimited = true
		}
		if apiErr.RetryAfter > 0 {
			retryAfter = time.Duration(apiErr.RetryAfter * float64(time.Second))
		}
	}
	if retryAfter == 0 && resp != nil && resp.RetryAfter > 0 {
		retryAfter = resp.RetryAfter
	}

	c.stateMu.Lock()
	c.failures++
	failures := c.failures
	c.stateMu.Unlock()

	delay := retryAfter
	if delay <= 0 {
		delay = backoffDelay(c.cfg.CacheTTL, 5*time.Minute, failures)
	}
	c.stateMu.Lock()
	c.notBefore = now.Add(delay)
	c.stateMu.Unlock()

	if rateLimited {
		c.warnOnce("snapshot-429", "snapshot refresh rate limited; waiting %s and serving the cached document", delay.Round(time.Millisecond))
	} else {
		c.warnOnce("snapshot-fail", "snapshot refresh failed (%v); retrying in %s and serving the cached document", err, delay.Round(time.Millisecond))
	}
	return delay
}

func (c *Client) persistDisk(snap *Snapshot, resp *snapshotResponse, fetchedAt time.Time) {
	if c.cfg.DisableDiskCache || c.cfg.DiskCachePath == "" {
		return
	}
	err := writeSnapshotFile(c.cfg.DiskCachePath, snap.Raw, sidecar{
		ETag:         resp.ETag,
		LastModified: resp.LastModified,
		Environment:  snap.Environment,
		Project:      snap.Project,
		FetchedAt:    fetchedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		c.warnOnce("disk-write", "could not write the snapshot disk cache at %s: %v", c.cfg.DiskCachePath, err)
	}
}

// ---------------------------------------------------------------------------
// POST /resolve

// ResolveRemote asks the server to resolve, which is the simple path and the
// smoke test for a deployment. It is not for a hot loop: the answer is cached
// for the same TTL as the snapshot, per use case, prompt and environment, and
// variables are rendered locally against that cached template. When the server
// rate limits, fails or is unreachable, the cached answer is served.
func (c *Client) ResolveRemote(ctx context.Context, useCase string, opts ...ResolveOption) (*Resolution, error) {
	o := buildResolveOptions(opts)
	env := o.Environment
	if env == "" {
		env = c.cfg.Environment
	}
	prompt := o.Prompt
	key := env + "\x00" + useCase + "\x00" + prompt

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
			UseCase:     useCase,
			Environment: env,
			Prompt:      prompt,
		})
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
				return nil, resolveErrorFromAPI(useCase, apiErr)
			}
			if hasCached {
				c.warnOnce("resolve-remote", "POST /resolve failed (%v); serving the cached answer", err)
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

	res := resolutionFromResponse(useCase, response)
	if o.Variables != nil {
		if err := renderInto(res, o.Variables); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func resolutionFromResponse(useCase string, r *resolveResponse) *Resolution {
	res := &Resolution{
		UseCase:            useCase,
		Kind:               Kind(r.Kind),
		DeploymentID:       r.Deployment.ID,
		DeploymentRevision: r.Deployment.Revision,
		Prompts:            r.Prompts,
		Params:             r.EffectiveParams,
		ProviderOptions:    r.EffectiveProviderOptions,
		Messages:           append([]Message(nil), r.Messages...),
		Source:             SourceRemote,
		ETag:               r.ETag,
		Warnings:           r.Warnings,
	}
	if res.Prompts == nil {
		res.Prompts = []string{}
	}
	if r.Prompt != nil {
		res.Prompt = *r.Prompt
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
	if r.PromptVersion != nil {
		res.PromptVersionID = r.PromptVersion.ID
		res.PromptVersionNumber = r.PromptVersion.Number
	}
	if r.Text != nil {
		res.Text = *r.Text
	}
	return res
}
