package prompton

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels for errors.Is. A 404 whose reason is unresolved or unknown_prompt
// is a bug in the deployment or in the call — never a signal to fall back to a
// hard-coded prompt.
var (
	// ErrNotReady means no snapshot is available at all: the server is
	// unreachable and neither the disk cache nor a bundle held a document.
	ErrNotReady = errors.New("prompton: no snapshot available (PromptOn is unreachable and nothing is cached)")

	// ErrUnknownUseCase means the snapshot has no use case with that key.
	ErrUnknownUseCase = errors.New("prompton: unknown use case")

	// ErrUnresolved means the use case exists but has no live deployment in
	// this environment.
	ErrUnresolved = errors.New("prompton: no live deployment for this use case")

	// ErrUnknownPrompt means the live revision pins no prompt version under the
	// requested name. There is no silent fallback to "default": shipping
	// English to a request that asked for "ko" is worse than an error.
	ErrUnknownPrompt = errors.New("prompton: unknown prompt name")

	// ErrNoTemplate means the resolution carries no template to render, which
	// is the normal state of an embedding use case.
	ErrNoTemplate = errors.New("prompton: use case has no prompt template")

	// ErrEnvironmentMismatch means a local resolve asked for an environment
	// other than the one the loaded document belongs to. Local resolution reads
	// one document and cannot answer for another environment; answering from
	// the wrong one silently is exactly the accident the guard prevents.
	ErrEnvironmentMismatch = errors.New("prompton: resolve asked for another environment")

	// ErrClosed is returned by a client whose Close has already run.
	ErrClosed = errors.New("prompton: client is closed")

	// ErrNoAPIKey means a remote call was attempted with no API key configured.
	ErrNoAPIKey = errors.New("prompton: no API key configured")
)

// UseCaseError is a resolution failure. Code is one of "unknown_use_case",
// "unresolved", "unknown_prompt" — matching the reason the server reports in
// the details of its 404 — or "environment_mismatch", which only local
// resolution can produce.
type UseCaseError struct {
	Code        string
	UseCase     string
	Prompt      string
	PromptNames []string

	// Environment and DocumentEnvironment are set for "environment_mismatch":
	// what the call asked for, and what the loaded document actually holds.
	Environment         string
	DocumentEnvironment string
}

func (e *UseCaseError) Error() string {
	switch e.Code {
	case "unknown_use_case":
		return fmt.Sprintf("prompton: unknown use case %q", e.UseCase)
	case "unresolved":
		return fmt.Sprintf("prompton: use case %q has no live deployment in this environment", e.UseCase)
	case "unknown_prompt":
		return fmt.Sprintf("prompton: use case %q pins no prompt named %q (available: %s)",
			e.UseCase, e.Prompt, strings.Join(e.PromptNames, ", "))
	case "environment_mismatch":
		return fmt.Sprintf("prompton: resolve asked for environment %q but this client reads %q; "+
			"use RemoteUseCase, or a second client configured for %q",
			e.Environment, e.DocumentEnvironment, e.Environment)
	default:
		return "prompton: " + e.Code
	}
}

// Unwrap maps the code onto a sentinel so errors.Is works.
func (e *UseCaseError) Unwrap() error {
	switch e.Code {
	case "unknown_use_case":
		return ErrUnknownUseCase
	case "unresolved":
		return ErrUnresolved
	case "unknown_prompt":
		return ErrUnknownPrompt
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

// useCaseErrorFromAPI turns a server 404 into the same UseCaseError local
// selection produces, so callers can branch on one error shape.
func useCaseErrorFromAPI(useCase string, e *APIError) error {
	if e.Status != 404 {
		return e
	}
	reason, _ := e.Details["reason"].(string)
	switch reason {
	case "unresolved":
		return &UseCaseError{Code: "unresolved", UseCase: useCase}
	case "unknown_prompt":
		prompt, _ := e.Details["prompt"].(string)
		var available []string
		if list, ok := e.Details["prompt_names"].([]interface{}); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					available = append(available, s)
				}
			}
		}
		return &UseCaseError{Code: "unknown_prompt", UseCase: useCase, Prompt: prompt, PromptNames: available}
	}
	if _, ok := e.Details["key"]; ok {
		return &UseCaseError{Code: "unknown_use_case", UseCase: useCase}
	}
	return e
}
