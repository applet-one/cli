// Package api is a client for the Applet API.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is used when APPLET_API_URL is not set.
const DefaultBaseURL = "https://api.applet.one/api/v1"

// Error is an error response from the Applet API.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// HasCode reports whether err is an API error with the given code.
func HasCode(err error, code string) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}

// Code returns the API error code of err, or "".
func Code(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// Client calls the Applet API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Token returns the bearer token for authenticated requests.
	Token     func() (string, error)
	UserAgent string
}

// RequestOption customizes a request.
type RequestOption func(*requestOptions)

type requestOptions struct {
	noAuth  bool
	headers map[string]string
	timeout time.Duration
}

// NoAuth sends the request without a bearer token.
func NoAuth() RequestOption { return func(o *requestOptions) { o.noAuth = true } }

// Header sets a request header.
func Header(key, value string) RequestOption {
	return func(o *requestOptions) { o.headers[key] = value }
}

// Timeout bounds the whole request.
func Timeout(d time.Duration) RequestOption { return func(o *requestOptions) { o.timeout = d } }

// IdempotencyKey sets a fresh idempotency key and returns it.
func IdempotencyKey(key string) RequestOption { return Header("idempotency-key", key) }

// Do sends a JSON request and decodes the response's data into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, path string, body, out any, options ...RequestOption) error {
	opts := requestOptions{headers: map[string]string{}}
	for _, option := range options {
		option(&opts)
	}
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json")
	if c.UserAgent != "" {
		request.Header.Set("user-agent", c.UserAgent)
	}
	if !opts.noAuth {
		token, err := c.Token()
		if err != nil {
			return err
		}
		request.Header.Set("authorization", "Bearer "+token)
	}
	for key, value := range opts.headers {
		request.Header.Set(key, value)
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Could not connect to the Applet API at %s (%s). Check your internet connection and APPLET_API_URL.", c.BaseURL, connectionDetail(err))
	}
	defer response.Body.Close()
	contentType := response.Header.Get("content-type")
	if !strings.Contains(contentType, "application/json") {
		if contentType == "" {
			contentType = "a response without a content type"
		}
		return fmt.Errorf("Expected a JSON response from the Applet API, but received %s. Check APPLET_API_URL; a proxy, login page, or Cloudflare Access may be intercepting the request.", contentType)
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("The Applet API returned invalid JSON (HTTP %d). Check APPLET_API_URL or try again later.", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		apiErr := &Error{Status: response.StatusCode, Message: fmt.Sprintf("The Applet API request failed (HTTP %d).", response.StatusCode)}
		if envelope.Error != nil {
			apiErr.Code = envelope.Error.Code
			if envelope.Error.Message != "" {
				apiErr.Message = envelope.Error.Message
			}
		}
		return apiErr
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("The Applet API returned an unexpected response: %w", err)
		}
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func connectionDetail(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return err.Error()
}

// PathEscape escapes a path segment.
func PathEscape(s string) string { return url.PathEscape(s) }

// NewIdempotencyKey returns a random UUIDv4.
func NewIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
