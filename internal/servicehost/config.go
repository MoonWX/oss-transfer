// Package servicehost connects the native programs to OS service supervisors.
package servicehost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/comparison"
)

type Config struct {
	NodeControl          *NodeControlConfig `json:"nodeControl,omitempty"`
	Comparison           *ComparisonConfig  `json:"comparison,omitempty"`
	ServiceName          string             `json:"serviceName"`
	DataDirectory        string             `json:"dataDirectory"`
	Listen               string             `json:"listen,omitempty"`
	ReadyFile            string             `json:"readyFile"`
	LogFile              string             `json:"logFile"`
	AgentArtifactBase    string             `json:"agentArtifactBase,omitempty"`
	AgentArtifactVersion string             `json:"agentArtifactVersion,omitempty"`
	AgentDockerImage     string             `json:"agentDockerImage,omitempty"`
}

// ComparisonConfig limits the object comparison results an Agent keeps under
// dataDirectory/comparisons. Every field is optional; 0 selects the default
// (2 GiB, 30 days, 1 GiB).
type ComparisonConfig struct {
	BudgetBytes      int64 `json:"budgetBytes,omitempty"`
	RetentionDays    int   `json:"retentionDays,omitempty"`
	MinimumFreeBytes int64 `json:"minimumFreeBytes,omitempty"`
}

// minimumComparisonBudgetBytes is the smallest nonzero comparison budget.
const minimumComparisonBudgetBytes = 64 << 20

// maximumComparisonRetentionDays bounds comparison.retentionDays.
const maximumComparisonRetentionDays = 3650

func (c *ComparisonConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.BudgetBytes != 0 && c.BudgetBytes < minimumComparisonBudgetBytes {
		return fmt.Errorf("comparison budgetBytes must be 0 or at least %d", minimumComparisonBudgetBytes)
	}
	if c.RetentionDays < 0 || c.RetentionDays > maximumComparisonRetentionDays {
		return fmt.Errorf("comparison retentionDays must be 0–%d", maximumComparisonRetentionDays)
	}
	if c.MinimumFreeBytes < 0 {
		return errors.New("comparison minimumFreeBytes must not be negative")
	}
	return nil
}

// Options maps the configured limits; directory holds the results. Zero values
// keep the comparison defaults.
func (c *ComparisonConfig) Options(directory string) comparison.Options {
	options := comparison.Options{Directory: directory}
	if c != nil {
		options.BudgetBytes, options.RetentionDays, options.MinimumFreeBytes = c.BudgetBytes, c.RetentionDays, c.MinimumFreeBytes
	}
	return options
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

func ValidName(name string) bool   { return namePattern.MatchString(name) }
func cleanValue(value string) bool { return !strings.ContainsAny(value, "\x00\r\n\t") }
func absolute(value string) bool {
	return filepath.IsAbs(value) && cleanValue(value) && filepath.Clean(value) == value
}

func LoadConfig(path, role string) (Config, error) {
	var config Config
	if role != "master" && role != "agent" {
		return config, errors.New("role must be master or agent")
	}
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("read service configuration: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return config, errors.New("service configuration must contain one JSON object")
	}
	if err = config.Validate(role); err != nil {
		return config, err
	}
	return config, nil
}
func (c Config) Validate(role string) error {
	if c.NodeControl != nil && role != "agent" {
		return errors.New("nodeControl belongs to Agent configuration")
	}
	if c.Comparison != nil && role != "agent" {
		return errors.New("comparison belongs to Agent configuration")
	}
	if err := c.Comparison.validate(); err != nil {
		return err
	}
	if _, err := c.NodeControl.Options(); err != nil {
		return err
	}
	if !ValidName(c.ServiceName) || !absolute(c.DataDirectory) || !absolute(c.ReadyFile) || !absolute(c.LogFile) {
		return errors.New("service name and absolute data/readiness/log paths are required")
	}
	if c.ReadyFile == c.LogFile || filepath.Dir(c.ReadyFile) != filepath.Join(c.DataDirectory, "runtime") || filepath.Dir(c.LogFile) != filepath.Join(c.DataDirectory, "logs") {
		return errors.New("readiness must be in dataDirectory/runtime and logs in dataDirectory/logs")
	}
	if role == "master" {
		host, port, err := net.SplitHostPort(c.Listen)
		if err != nil || !cleanValue(c.Listen) || strings.ContainsAny(host, "\"'\\ ") {
			return errors.New("listen must be a TCP host:port")
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return errors.New("listen port must be between 1 and 65535")
		}
	} else if role != "agent" || c.Listen != "" {
		return errors.New("Agent configuration must not include a listen address")
	}
	return nil
}

// InspectConfig emits a fixed, non-executable format for the platform installers.
// It contains no account, enrollment or storage credentials.
func InspectConfig(w io.Writer, c Config) error {
	for _, field := range [][2]string{{"serviceName", c.ServiceName}, {"dataDirectory", c.DataDirectory}, {"listen", c.Listen}, {"readyFile", c.ReadyFile}, {"logFile", c.LogFile}} {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", field[0], field[1]); err != nil {
			return err
		}
	}
	return nil
}
