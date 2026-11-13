package master

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"

	"github.com/MoonWX/oss-transfer/internal/auth"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// initialAlphabet has only letters and digits: a terminal's double-click
// selection stops at "-" or "_", so a password containing them was copied
// incompletely and then rejected at the first sign-in.
const initialAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// initialPasswordLength characters of 62 give about 143 bits, more than the
// 18 random bytes used before.
const initialPasswordLength = 24

func initialCredential() (password, hash string, err error) {
	characters := make([]byte, initialPasswordLength)
	limit := big.NewInt(int64(len(initialAlphabet)))
	for i := range characters {
		index, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", "", err
		}
		characters[i] = initialAlphabet[index.Int64()]
	}
	password = string(characters)
	hash, err = auth.HashPassword(password)
	return
}

func (s *Server) initializeAdministrator() (string, error) {
	password, hash, err := initialCredential()
	if err != nil {
		return "", err
	}
	created := false
	err = s.store.Update(func(tx *state.Tx) error {
		var config Configuration
		if _, err := tx.Get("system", "configuration", &config); err != nil {
			return err
		}
		if config.Configured {
			return nil
		}
		if err := requireEmptyAccounts(tx); err != nil {
			return err
		}
		if err := state.RequireSchema(tx, "master", state.SchemaInitialPassword); err != nil {
			return err
		}
		user := User{
			Username: "admin", DisplayName: "admin", Role: "admin",
			PasswordHash: hash, Revision: 1, MustChangePassword: true,
		}
		if err := tx.Put("users", user.Username, user); err != nil {
			return err
		}
		created = true
		return tx.Put("system", "configuration", Configuration{
			Configured: true, CreatedAt: s.options.Now().UTC(),
		})
	})
	if err != nil || !created {
		return "", err
	}
	return password, nil
}

// ResetAdministratorPassword is an offline recovery operation. The caller must
// hold the exclusive database lock, which prevents changes to a running Master.
// It preserves the account and all application data, revoking previous sessions
// through the account revision. Passwords are never stored in plaintext.
func ResetAdministratorPassword(store *state.Store, username string) (string, string, error) {
	if err := store.ClaimRole("master"); err != nil {
		return "", "", err
	}
	password, hash, err := initialCredential()
	if err != nil {
		return "", "", err
	}
	var selected User
	err = store.Update(func(tx *state.Tx) error {
		var config Configuration
		if _, err := tx.Get("system", "configuration", &config); err != nil {
			return err
		}
		if !config.Configured {
			return errors.New("no administrator to reset")
		}
		if username != "" {
			found, err := tx.Get("users", username, &selected)
			if err != nil {
				return err
			}
			if !found || selected.Role != "admin" {
				return errors.New("administrator not found")
			}
		} else {
			after := ""
			for {
				count := 0
				if err := tx.Scan("users", "", after, 100, func(key string, raw json.RawMessage) error {
					after = key
					count++
					var user User
					if err := json.Unmarshal(raw, &user); err != nil {
						return err
					}
					if user.Role == "admin" {
						if selected.Username != "" {
							return errors.New("multiple administrators; select one with --username")
						}
						selected = user
					}
					return nil
				}); err != nil {
					return err
				}
				if count < 100 {
					break
				}
			}
			if selected.Username == "" {
				return errors.New("administrator not found")
			}
		}
		selected.PasswordHash = hash
		selected.MustChangePassword = true
		selected.Revision++
		if err := state.RequireSchema(tx, "master", state.SchemaInitialPassword); err != nil {
			return err
		}
		return tx.Put("users", selected.Username, selected)
	})
	if err != nil {
		return "", "", err
	}
	return selected.Username, password, nil
}
