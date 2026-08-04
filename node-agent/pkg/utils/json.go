package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// FetchText fetches plain text from a URL and returns the body as a string.
func FetchText(ctx context.Context, url string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("unable to create GET request: %w", err)
	}

	slog.Info("Fetching text data", "url", url)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("unable to perform GET request: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("unable to read response body: %w", err)
	}

	return string(body), nil
}

// FetchJSON fetches JSON data from a URL and decodes it into the target interface
func FetchJSON(ctx context.Context, url string, target interface{}) error {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("unable to create GET request: %w", err)
	}

	slog.Info("Fetching JSON data", "url", url)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unable to perform GET request: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("unable to decode JSON response: %w", err)
	}

	return nil
}
