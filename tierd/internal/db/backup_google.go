package db

import "time"

type BackupGoogleAccount struct {
	ID           int64  `json:"id"`
	Label        string `json:"label"`
	Email        string `json:"email"`
	ClientID     string `json:"-"`
	ClientSecret string `json:"-"`
	RefreshToken string `json:"-"`
	CreatedAt    string `json:"created_at"`
	Jobs         int    `json:"jobs"`
}

func (s *Store) CreateBackupGoogleAccount(a BackupGoogleAccount) (*BackupGoogleAccount, error) {
	a.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`INSERT INTO backup_google_accounts(label,email,client_id,client_secret,refresh_token,created_at) VALUES(?,?,?,?,?,?)`, a.Label, a.Email, a.ClientID, a.ClientSecret, a.RefreshToken, a.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	a.ID, err = res.LastInsertId()
	return &a, err
}
func (s *Store) ListBackupGoogleAccounts() ([]BackupGoogleAccount, error) {
	rows, err := s.db.Query(`SELECT id,label,email,created_at,(SELECT count(*) FROM backup_configs WHERE google_account_id=a.id) FROM backup_google_accounts a ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []BackupGoogleAccount{}
	for rows.Next() {
		var a BackupGoogleAccount
		if err := rows.Scan(&a.ID, &a.Label, &a.Email, &a.CreatedAt, &a.Jobs); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}
func (s *Store) GetBackupGoogleAccount(id int64) (*BackupGoogleAccount, error) {
	var a BackupGoogleAccount
	err := s.db.QueryRow(`SELECT id,label,email,client_id,client_secret,refresh_token,created_at FROM backup_google_accounts WHERE id=?`, id).Scan(&a.ID, &a.Label, &a.Email, &a.ClientID, &a.ClientSecret, &a.RefreshToken, &a.CreatedAt)
	return &a, err
}
func (s *Store) DeleteBackupGoogleAccount(id int64) error {
	res, err := s.db.Exec(`DELETE FROM backup_google_accounts WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) ClaimBackupSlot(id int64, previous, slot string) (bool, error) {
	res, err := s.db.Exec(`UPDATE backup_configs SET last_scheduled_at=? WHERE id=? AND schedule_enabled=1 AND last_scheduled_at=?`, slot, id, previous)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
