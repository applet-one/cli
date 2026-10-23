package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Namespace is an Applet namespace.
type Namespace struct {
	Slug     string `json:"slug"`
	AppLimit *int   `json:"app_limit"`
}

// Me is the authenticated identity.
type Me struct {
	Namespaces []json.RawMessage `json:"namespaces"`
}

// App is a hosted app.
type App struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	URL    string `json:"url"`
	Access string `json:"access"`
}

// Deployment is a deployment record.
type Deployment struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"error_message"`
}

// Operation is a long-running operation.
type Operation struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Phase        string  `json:"phase"`
	ErrorMessage *string `json:"error_message"`
}

// FirstNamespace returns the caller's first namespace, its raw JSON, or
// ok=false when there is none.
func (c *Client) FirstNamespace(ctx context.Context) (Namespace, json.RawMessage, bool, error) {
	var me Me
	if err := c.Do(ctx, http.MethodGet, "/me", nil, &me); err != nil {
		return Namespace{}, nil, false, err
	}
	if len(me.Namespaces) == 0 || string(me.Namespaces[0]) == "null" {
		return Namespace{}, nil, false, nil
	}
	var namespace Namespace
	if err := json.Unmarshal(me.Namespaces[0], &namespace); err != nil {
		return Namespace{}, nil, false, err
	}
	return namespace, me.Namespaces[0], namespace.Slug != "", nil
}

// NamespaceSlug returns the caller's first namespace slug or fails with
// missing as the error message.
func (c *Client) NamespaceSlug(ctx context.Context, missing string) (string, error) {
	namespace, _, ok, err := c.FirstNamespace(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New(missing)
	}
	return namespace.Slug, nil
}

// AppPath returns the API path for an app.
func AppPath(namespace, app string) string {
	return "/namespaces/" + PathEscape(namespace) + "/apps/" + PathEscape(app)
}

// GetApp fetches an app.
func (c *Client) GetApp(ctx context.Context, namespace, app string) (App, error) {
	var result struct {
		App App `json:"app"`
	}
	err := c.Do(ctx, http.MethodGet, AppPath(namespace, app), nil, &result)
	return result.App, err
}
