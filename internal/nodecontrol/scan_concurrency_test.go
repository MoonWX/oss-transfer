package nodecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func scanConcurrencyValue(value int) *int { return &value }

// storedScanConcurrency reads the settings record as stored.
func storedScanConcurrency(t *testing.T, db *state.Store) *int {
	t.Helper()
	var saved persisted
	if err := db.View(func(tx *state.Tx) error {
		_, err := tx.Get("node-control", "settings", &saved)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return saved.ScanConcurrency
}

func TestScanConcurrencySavedAndRestored(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	if config := f.controller.Config(); config.MaxConcurrentScans != DefaultScanConcurrency {
		t.Fatalf("a new node runs %d scans, want the default %d", config.MaxConcurrentScans, DefaultScanConcurrency)
	}
	if _, saved := f.controller.SavedScanConcurrency(); saved {
		t.Fatal("a new node has a saved scan concurrency")
	}
	config, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentScans: scanConcurrencyValue(5)})
	must(t, err)
	if config.MaxConcurrentScans != 5 {
		t.Fatalf("the change returned %d scans", config.MaxConcurrentScans)
	}
	for _, invalid := range []int{0, -1, MaxScanConcurrency + 1} {
		if _, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentScans: scanConcurrencyValue(invalid)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("scan concurrency %d accepted: %v", invalid, err)
		}
	}
	// A change that fails on another field saves nothing.
	if _, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentScans: scanConcurrencyValue(7), AdvertisedLocalRoots: &[]string{"relative"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid roots accepted: %v", err)
	}
	f.restart(t, func(*Options) {})
	if scans, saved := f.controller.SavedScanConcurrency(); !saved || scans != 5 {
		t.Fatalf("restart restored %d (saved %v), want 5", scans, saved)
	}
	if config := f.controller.Config(); config.MaxConcurrentScans != 5 {
		t.Fatalf("restart reports %d scans, want 5", config.MaxConcurrentScans)
	}
	for _, valid := range []int{1, MaxScanConcurrency} {
		config, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentScans: scanConcurrencyValue(valid)})
		if err != nil || config.MaxConcurrentScans != valid {
			t.Fatalf("scan concurrency %d: %d %v", valid, config.MaxConcurrentScans, err)
		}
	}
}

// A saved scan concurrency outside the bounds, which a later version could
// have saved before a downgrade, is logged and dropped; the Agent starts with
// the default instead of refusing to start.
func TestInvalidSavedScanConcurrencyIsIgnored(t *testing.T) {
	for _, invalid := range []int{0, -3, MaxScanConcurrency + 1, 64} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			var mu sync.Mutex
			var lines []string
			f := newFixture(t, Options{Logf: func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, fmt.Sprintf(format, args...))
			}})
			must(t, f.controller.Close())
			f.controller = nil
			must(t, f.db.Update(func(tx *state.Tx) error {
				var saved persisted
				if _, err := tx.Get("node-control", "settings", &saved); err != nil {
					return err
				}
				saved.ScanConcurrency = scanConcurrencyValue(invalid)
				saved.Concurrency = &ConcurrencyConfig{MaxConcurrentChildren: 8, ObjectsPerChild: 2}
				return tx.Put("node-control", "settings", saved)
			}))
			var err error
			f.controller, err = New(f.db, f.limits, f.options)
			if err != nil {
				t.Fatalf("an invalid saved scan concurrency stopped the start: %v", err)
			}
			if config := f.controller.Config(); config.MaxConcurrentScans != DefaultScanConcurrency || config.MaxConcurrentChildren != 8 || config.ObjectsPerChild != 2 {
				t.Fatalf("started with %+v, want the default scans and the saved child concurrency", config)
			}
			if _, saved := f.controller.SavedScanConcurrency(); saved {
				t.Fatal("the invalid value still counts as saved")
			}
			if stored := storedScanConcurrency(t, f.db); stored != nil {
				t.Fatalf("the invalid value %d stayed in the database", *stored)
			}
			mu.Lock()
			defer mu.Unlock()
			want := fmt.Sprintf("ignoring invalid saved scan concurrency %d; using %d", invalid, DefaultScanConcurrency)
			if len(lines) != 1 || lines[0] != want {
				t.Fatalf("logged %q, want %q", lines, want)
			}
		})
	}
}

// Settings saved by an Agent before scan concurrency have no such field, and
// an older Agent that reads settings with one keeps every other setting and
// drops it when it saves them on start, as nodecontrol.New does.
func TestOlderSettingsHaveNoScanConcurrency(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	children, objects := 12, 3
	_, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentChildren: &children, ObjectsPerChild: &objects, MaxConcurrentScans: scanConcurrencyValue(6)})
	must(t, err)
	must(t, f.controller.Close())
	f.controller = nil
	must(t, f.db.Update(func(tx *state.Tx) error {
		var raw json.RawMessage
		if _, err := tx.Get("node-control", "settings", &raw); err != nil {
			return err
		}
		// The previous release's record: the same fields without
		// scanConcurrency, decoded leniently and saved again.
		var older struct {
			Overrides      map[string]float64    `json:"overrides"`
			LocalOverrides map[string]float64    `json:"localOverrides,omitempty"`
			Effective      map[string]float64    `json:"effective"`
			Last           map[string]Adjustment `json:"last"`
			Incidents      map[string]string     `json:"incidents"`
			Roots          []string              `json:"advertisedLocalRoots"`
			Sequence       uint64                `json:"sequence"`
			Concurrency    *ConcurrencyConfig    `json:"concurrency,omitempty"`
		}
		if err := json.Unmarshal(raw, &older); err != nil {
			return err
		}
		return tx.Put("node-control", "settings", older)
	}))
	f.controller, err = New(f.db, f.limits, f.options)
	must(t, err)
	if _, saved := f.controller.SavedScanConcurrency(); saved {
		t.Fatal("settings without scanConcurrency report a saved value")
	}
	config := f.controller.Config()
	if config.MaxConcurrentScans != DefaultScanConcurrency || config.MaxConcurrentChildren != 12 || config.ObjectsPerChild != 3 {
		t.Fatalf("older settings read as %+v", config)
	}
}

// Scan concurrency and child concurrency are saved apart: changing one does
// not save the other's default as a choice.
func TestScanConcurrencyChangeLeavesChildConcurrencyUnset(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	_, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentScans: scanConcurrencyValue(4)})
	must(t, err)
	if _, saved := f.controller.SavedConcurrency(); saved {
		t.Fatal("a scan concurrency change saved the child concurrency")
	}
	children := 16
	config, err := f.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentChildren: &children})
	must(t, err)
	if config.MaxConcurrentScans != 4 {
		t.Fatalf("a child concurrency change reset the scans to %d", config.MaxConcurrentScans)
	}
	g := newFixture(t, Options{})
	_, err = g.controller.UpdateConfig(ctx, ConfigChange{MaxConcurrentChildren: &children})
	must(t, err)
	if _, saved := g.controller.SavedScanConcurrency(); saved {
		t.Fatal("a child concurrency change saved the scan concurrency")
	}
	if stored := storedScanConcurrency(t, g.db); stored != nil {
		t.Fatalf("a child concurrency change stored scan concurrency %d", *stored)
	}
}
