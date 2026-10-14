package prompton

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels for errors.Is. A 404 whose reason is unresolved or unknown_template
// is a bug in the deployment or in the call — never a signal to fall back to a
// hard-coded prompt.
var (
	// ErrNotReady means no snapshot is available at all: the server is
	// unreachable and neither the disk cache nor a bundle held a document.
	ErrNotReady = errors.New("prompton: no snapshot available (PromptOn is unreachable and nothing is cached)")

	// ErrUnknownPrompt means the snapshot has no prompt with that key.
	ErrUnknownPrompt = errors.New("prompton: unknown prompt")

	// ErrUnresolved means the prompt exists but has no live deployment in
	// this environment.
	ErrUnresolved = errors.New("prompton: no live deployment for this prompt")

	// ErrUnknownTemplate means the live revision pins no prompt version under the
	// requested template name. There is no silent fallback to "default".
	ErrUnknownTemplate = errors.New("prompton: unknown template name")

	// ErrNoTemplate means the resolution carries no template to render, which
	// is the normal state of an embedding prompt.
	ErrNoTemplate = errors.New("prompton: prompt has no prompt template")

	// ErrEnvironmentMismatch means a local resolve asked for an environment
	// other than the one the loaded document belongs to.
	ErrEnvironmentMismatch = errors.New("prompton: resolve asked for another environment")

	// ErrClosed is returned by a client whose Close has already run.
	ErrClosed = errors.New("prompton: client is closed")

	// ErrNoAPIKey means a remote call was attempted with no API key configured.
	ErrNoAPIKey = errors.New("prompton: no API key configured")
)

// PromptError is a resolution failure. Code is one of "unknown_prompt",
// "unresolved", "unknown_template", or "environment_mismatch".
type PromptError struct {
	Code          string
	Prompt        string
	Template      string
	TemplateNames []string

	// Environment and DocumentEnvironment are set for "environment_mismatch".
	Environment         string
	DocumentEnvironment string
}

func (e *PromptError) Error() string {
	switch e.Code {
	case "unknown_prompt":
		return fmt.Sprintf("prompton: unknown prompt %q", e.Prompt)
	case "unresolved":
		return fmt.Sprintf("prompton: prompt %q has no live deployment in this environment", e.Prompt)
	case "unknown_template":
		return fmt.Sprintf("prompton: prompt %q pins no template named %q (available: %s)",
			e.Prompt, e.Template, strings.Join(e.TemplateNames, ", "))
	case "environment_mismatch":
		return fmt.Sprintf("prompton: resolve asked for environment %q but this client reads %q; "+
			"use RenderPrompt, or a second client configured for %q",
			e.Environment, e.DocumentEnvironment, e.Environment)
	default:
		return "prompton: " + e.Code
	}
}

// Unwrap maps the code onto a sentinel so errors.Is works.
func (e *PromptError) Unwrap() error {
	switch e.Code {
	case "unknown_prompt":
		return ErrUnknownPrompt
	case "unresolved":
		return ErrUnresolved
	case "unknown_template":
		return ErrUnknownTemplate
	case "environment_mismatch":
		return ErrEnvironmentMismatch
	}
	return nil
}

// MissingVariableError says the template needed a variable this call did not
// send. Variable is the dotted path as the template wrote it.
type MissingVariableError struct {
	Variable string
}

func (e *MissingVariableError) Error() string {
	return fmt.Sprintf("prompton: missing variable %q", e.Variable)
}

// TemplateError is a template that failed to parse or to render for a reason
// other than a missing variable.
type TemplateError struct {
	Category string
	Message  string
}

func (e *TemplateError) Error() string {
	return fmt.Sprintf("prompton: template %s: %s", e.Category, e.Message)
}

// APIError is a non-success response from the PromptOn API.
type APIError struct {
	Status     int
	Code       string
	Message    string
	Details    map[string]interface{}
	RetryAfter float64 // seconds, when the response asked for a wait
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("prompton: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("prompton: HTTP %d", e.Status)
}

// promptErrorFromAPI turns a server 404 into the same PromptError local
// selection produces, so callers can branch on one error shape.
func promptErrorFromAPI(prompt string, e *APIError) error {
	if e.Status != 404 {
		return e
	}
	reason, _ := e.Details["reason"].(string)
	switch reason {
	case "unresolved":
		return &PromptError{Code: "unresolved", Prompt: prompt}
	case "unknown_template":
		template, _ := e.Details["template"].(string)
		var available []string
		if list, ok := e.Details["template_names"].([]interface{}); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					available = append(available, s)
				}
			}
		}
		return &PromptError{Code: "unknown_template", Prompt: prompt, Template: template, TemplateNames: available}
	}
	if _, ok := e.Details["key"]; ok {
		return &PromptError{Code: "unknown_template", Prompt: prompt}
	}
	return e
}
