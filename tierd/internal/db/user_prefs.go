package db

import (
	"database/sql"
	"errors"
)

// GetUserLanguage returns the language code stored for the given
// user. Empty string means "no preference recorded" (the caller
// should fall back to the system default / UI fallback chain).
//
// The username is keyed exactly as the auth layer stores it; we do
// not normalise case or trim whitespace.
func (s *Store) GetUserLanguage(username string) (string, error) {
	if username == "" {
		return "", nil
	}
	var lang string
	err := s.db.QueryRow(
		`SELECT language FROM user_prefs WHERE username = ?`,
		username,
	).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return lang, nil
}

// SetUserLanguage upserts the language code for a user. Storing an
// empty string clears the preference (subsequent reads return the
// "no preference" empty string).
func (s *Store) SetUserLanguage(username, language string) error {
	if username == "" {
		return errors.New("user_prefs: empty username")
	}
	_, err := s.db.Exec(
		`INSERT INTO user_prefs (username, language)
		 VALUES (?, ?)
		 ON CONFLICT(username) DO UPDATE SET
		   language = excluded.language,
		   updated_at = CURRENT_TIMESTAMP`,
		username, language,
	)
	return err
}
