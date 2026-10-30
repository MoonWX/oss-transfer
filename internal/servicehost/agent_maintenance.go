package servicehost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentUpgradeMaintenance runs before stopping the Agent, so it uses its
// private local grant instead of opening the running service's database.
func AgentUpgradeMaintenance(ctx context.Context, config Config, action, receiptPath string) error {
	return agentUpgradeMaintenance(ctx, config, action, receiptPath, nil)
}

func readMaintenanceJSON(path string, into any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 {
		return errors.New("invalid Agent maintenance file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		return errors.New("Agent maintenance file changed while opening")
	}
	if err := privateMaintenanceFile(path, current); err != nil {
		return errors.New("Agent maintenance file permissions are not private")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	if decoder.Decode(into) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid Agent maintenance file contents")
	}
	return nil
}

func agentUpgradeMaintenance(ctx context.Context, config Config, action, receiptPath string, transport http.RoundTripper) error {
	if (action != "begin" && action != "end") || !filepath.IsAbs(receiptPath) {
		return errors.New("Agent maintenance requires begin or end and an absolute receipt path")
	}
	var auth struct {
		MasterURL string `json:"masterUrl"`
		Role      string `json:"role"`
		Token     string `json:"token"`
	}
	if err := readMaintenanceJSON(filepath.Join(config.DataDirectory, "cli-local-auth.json"), &auth); err != nil {
		return fmt.Errorf("cannot protect Agent upgrade: local access is unavailable; upgrade the Master and ensure this Agent is connected first: %w", err)
	}
	origin, err := url.Parse(auth.MasterURL)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Hostname() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || auth.Role != "agent" || len(auth.Token) < 32 || len(auth.Token) > 4096 || strings.ContainsAny(auth.Token, " \r\n\t") {
		return errors.New("invalid local Agent access configuration")
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if err := readMaintenanceJSON(receiptPath, &receipt); err != nil {
		if action != "begin" || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		receipt.ID = hex.EncodeToString(random[:])
		file, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if err = privateFile(file); err == nil {
			err = json.NewEncoder(file).Encode(receipt)
		}
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
	}
	if id, err := hex.DecodeString(receipt.ID); err != nil || len(id) != 32 {
		return errors.New("invalid Agent maintenance receipt")
	}
	body, _ := json.Marshal(map[string]string{"action": action, "id": receipt.ID})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.MasterURL+"/api/master/agents/maintenance", bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Agent maintenance request")
	}
	request.Header.Set("Authorization", "Bearer "+auth.Token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if action == "begin" {
			return errors.New("cannot contact the Master to protect this Agent upgrade; the service has not been stopped")
		}
		return errors.New("cannot contact the Master to end Agent upgrade maintenance")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errors.New("the Master does not support upgrade maintenance; upgrade the Master before upgrading this Agent")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Agent upgrade maintenance rejected by Master (HTTP %d)", response.StatusCode)
	}
	var result struct {
		Success   bool      `json:"success"`
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16384))
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || !result.Success || result.ID != receipt.ID || result.ExpiresAt.IsZero() {
		return errors.New("invalid Master maintenance acknowledgement")
	}
	return nil
}
