package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Local CLI access is optional to the execution connection. Older Masters can
// keep running tasks while this Agent waits for the administration endpoint.
func (a *Agent) publishCLIAuth(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		token, err := a.fetchCLIAuth(ctx)
		if err == nil && ctx.Err() == nil {
			if err := a.options.OnCLIAuth(token); err == nil {
				return
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, time.Minute)
	}
}

func (a *Agent) fetchCLIAuth(ctx context.Context) (string, error) {
	if a.identity.EnrollmentID == "" {
		return "", errors.New("local CLI access requires an enrolled Agent")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.identity.MasterURL+"/api/agents/cli-access", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("X-Agent-Key", a.identity.Credential)
	client := http.Client{Transport: a.options.HTTPTransport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("local CLI access: HTTP %d", response.StatusCode)
	}
	var result struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	// Never propagate the response body or parsing errors: either could contain
	// credentials and eventually reach a service log.
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16*1024))
	if err := decoder.Decode(&result); err != nil || decoder.Decode(new(any)) != io.EOF || !result.Success || result.Token == "" || len(result.Token) > 8192 || strings.ContainsAny(result.Token, " \t\r\n") || result.Token == a.identity.Credential {
		return "", errors.New("invalid local CLI access response")
	}
	return result.Token, nil
}
