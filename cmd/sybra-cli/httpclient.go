package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
)

const apiCallTimeout = 15 * time.Second

const maxAPIResponseBody = 32 << 20

// serverTargetEnv is the explicit control-plane target for local CLI API mode.
// Reusing SYBRA_PORT/SYBRA_HOST here is unsafe: they describe where the server
// listens, and ambient unit-shell exports made the CLI hit unrelated localhost
// servers during ordinary filesystem-backed commands.
const serverTargetEnv = "SYBRA_SERVER_TARGET"

type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("server returned %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("server returned %d: %s", e.Status, e.Message)
}

func newAPIClient(cfg *config.Config) (client *apiClient, ok bool) {
	if cfg == nil || strings.TrimSpace(cfg.Server.AuthToken) == "" || cfg.ServesTLS() {
		return nil, false
	}
	host, port, ok := resolveDialTarget()
	if !ok {
		return nil, false
	}
	return &apiClient{
		baseURL: "http://" + net.JoinHostPort(host, port),
		token:   cfg.Server.AuthToken,
		http:    &http.Client{Timeout: apiCallTimeout},
	}, true
}

func resolveDialTarget() (host, port string, ok bool) {
	return parseServerTarget(os.Getenv(serverTargetEnv))
}

func parseServerTarget(raw string) (host, port string, ok bool) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", "", false
	}
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "http" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false
		}
		if path := strings.TrimSpace(u.EscapedPath()); path != "" && path != "/" {
			return "", "", false
		}
		target = u.Host
	}
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", "", false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	if strings.TrimSpace(h) == "" || isWildcardHost(h) {
		return "", "", false
	}
	return h, p, true
}

func isWildcardHost(h string) bool {
	return h == "0.0.0.0" || h == "::"
}

func (c *apiClient) reachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK
}

func (c *apiClient) call(ctx context.Context, service, method string, out any, args ...any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/"+service+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return parseAPIError(resp.StatusCode, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func parseAPIError(status int, body []byte) *apiError {
	e := &apiError{Status: status, Message: strings.TrimSpace(string(body))}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		e.Message = env.Error
		e.Code = env.Code
	}
	return e
}

func viaAPI[T any](api *apiClient, service, method string, args ...any) (result T, handled bool, err error) {
	if api == nil {
		return result, false, nil
	}
	callErr := api.call(context.Background(), service, method, &result, args...)
	if callErr == nil {
		return result, true, nil
	}
	var ae *apiError
	if errors.As(callErr, &ae) {
		return result, true, callErr
	}
	return result, false, nil
}
