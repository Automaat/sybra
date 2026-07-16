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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
)

const apiCallTimeout = 15 * time.Second

const maxAPIResponseBody = 32 << 20

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

func (e *apiError) isClientError() bool {
	return e.Status >= 400 && e.Status < 500
}

func newAPIClient(cfg *config.Config) (client *apiClient, ok bool) {
	if cfg == nil || strings.TrimSpace(cfg.Server.AuthToken) == "" || cfg.ServesTLS() {
		return nil, false
	}
	addrs, _ := cfg.ListenAddrs(os.Getenv("SYBRA_HOST"), os.Getenv("SYBRA_PORT"))
	if len(addrs) == 0 {
		return nil, false
	}
	_, portStr, err := net.SplitHostPort(addrs[0])
	if err != nil {
		return nil, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, false
	}
	return &apiClient{
		baseURL: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		token:   cfg.Server.AuthToken,
		http:    &http.Client{Timeout: apiCallTimeout},
	}, true
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
	if errors.As(callErr, &ae) && ae.isClientError() {
		return result, true, callErr
	}
	return result, false, nil
}
