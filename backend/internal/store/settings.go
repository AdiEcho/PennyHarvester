package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

func (s *SQLite) LoadSettings(ctx context.Context) (engine.Settings, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM settings WHERE id = 1").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		settings := engine.DefaultSettings()
		return settings, s.SaveSettings(ctx, settings)
	}
	if err != nil {
		return engine.Settings{}, err
	}
	// Start from defaults so a payload written by an older version that lacks
	// a newly added field remains readable instead of failing validation.
	settings := engine.DefaultSettings()
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.Settings{}, fmt.Errorf("读取策略设置: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return engine.Settings{}, fmt.Errorf("数据库中的策略设置无效: %w", err)
	}
	// A payload without fee_model_version was written before paper accounts
	// charged a fee. Its fee_bps of 0 is the old default, not a deliberate
	// choice, so the current default is applied once.
	var stored struct {
		FeeModelVersion int `json:"fee_model_version"`
	}
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		return engine.Settings{}, fmt.Errorf("读取策略设置: %w", err)
	}
	if stored.FeeModelVersion >= engine.FeeModelVersion {
		return settings, nil
	}
	if settings.FeeBPS == 0 {
		settings.FeeBPS = engine.DefaultSettings().FeeBPS
	}
	settings.FeeModelVersion = engine.FeeModelVersion
	if err := s.SaveSettings(ctx, settings); err != nil {
		return engine.Settings{}, err
	}
	return settings, nil
}

func (s *SQLite) SaveSettings(ctx context.Context, settings engine.Settings) error {
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(id, payload, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		string(payload), formatTime(time.Now().UTC()))
	return err
}

// LoadAccountSettings reads one account's strategy block. Accounts that never
// saved their own settings inherit the global defaults; the fallback is a live
// read so a fresh installation keeps working without a migration copying rows
// into every account.
func (s *SQLite) LoadAccountSettings(ctx context.Context, accountID int64) (engine.Settings, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM account_settings WHERE account_id = ?", accountID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return s.LoadSettings(ctx)
	}
	if err != nil {
		return engine.Settings{}, err
	}
	// Start from defaults so a payload written by an older version that lacks
	// a newly added field stays readable.
	settings := engine.DefaultSettings()
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.Settings{}, fmt.Errorf("读取账户策略设置: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return engine.Settings{}, fmt.Errorf("账户 %d 的策略设置无效: %w", accountID, err)
	}
	return settings, nil
}

func (s *SQLite) SaveAccountSettings(ctx context.Context, accountID int64, settings engine.Settings) error {
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO account_settings(account_id, payload, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		accountID, string(payload), formatTime(time.Now().UTC()))
	return err
}

func (s *SQLite) LoadSystemSettings(ctx context.Context) (engine.SystemSettings, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM system_settings WHERE id = 1").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		settings := engine.DefaultSystemSettings()
		return settings, s.SaveSystemSettings(ctx, settings)
	}
	if err != nil {
		return engine.SystemSettings{}, err
	}
	// Start from defaults so a payload written before a field existed stays
	// readable instead of failing validation.
	settings := engine.DefaultSystemSettings()
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	legacyPlaintext := false
	decrypt := func(value string) (string, error) {
		plain, legacy, err := s.decryptSetting(value)
		if err != nil {
			return "", err
		}
		if legacy {
			legacyPlaintext = true
		}
		return plain, nil
	}
	if settings.Proxy.URL, err = decrypt(settings.Proxy.URL); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.PrivateKey, err = decrypt(settings.Live.PrivateKey); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.APIKey, err = decrypt(settings.Live.APIKey); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.APISecret, err = decrypt(settings.Live.APISecret); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.APIPassphrase, err = decrypt(settings.Live.APIPassphrase); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	for v, entry := range settings.Venues {
		if entry.Live.PrivateKey, err = decrypt(entry.Live.PrivateKey); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		if entry.Live.APIKey, err = decrypt(entry.Live.APIKey); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		if entry.Live.APISecret, err = decrypt(entry.Live.APISecret); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		if entry.Live.APIPassphrase, err = decrypt(entry.Live.APIPassphrase); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		settings.Venues[v] = entry
	}
	settings = settings.Normalize()
	if err := settings.Validate(); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("数据库中的系统设置无效: %w", err)
	}
	if legacyPlaintext {
		// Upgrade rows written before secrets were encrypted at rest.
		if err := s.SaveSystemSettings(ctx, settings); err != nil {
			return engine.SystemSettings{}, err
		}
	}
	return settings, nil
}

func (s *SQLite) decryptSetting(value string) (string, bool, error) {
	if s.secrets == nil {
		return value, false, nil
	}
	plain, encrypted, err := s.secrets.Decrypt(value)
	if err != nil {
		if s.secrets.keyPath != "" {
			return "", true, fmt.Errorf("%w（请确认密钥文件 %s 与数据库一同保留；如确认要丢弃已保存的网页凭据，可删除 system_settings 表中的该行后重启）", err, s.secrets.keyPath)
		}
		return "", true, err
	}
	return plain, value != "" && !encrypted, nil
}

func (s *SQLite) SaveSystemSettings(ctx context.Context, settings engine.SystemSettings) error {
	stored := settings
	if s.secrets != nil {
		var err error
		if stored.Proxy.URL, err = s.secrets.Encrypt(stored.Proxy.URL); err != nil {
			return fmt.Errorf("加密代理设置: %w", err)
		}
		if stored.Live.PrivateKey, err = s.secrets.Encrypt(stored.Live.PrivateKey); err != nil {
			return fmt.Errorf("加密实盘私钥: %w", err)
		}
		if stored.Live.APIKey, err = s.secrets.Encrypt(stored.Live.APIKey); err != nil {
			return fmt.Errorf("加密实盘 API Key: %w", err)
		}
		if stored.Live.APISecret, err = s.secrets.Encrypt(stored.Live.APISecret); err != nil {
			return fmt.Errorf("加密 L2 API Secret: %w", err)
		}
		if stored.Live.APIPassphrase, err = s.secrets.Encrypt(stored.Live.APIPassphrase); err != nil {
			return fmt.Errorf("加密 L2 API Passphrase: %w", err)
		}
		if stored.Venues != nil {
			venues := make(map[market.Venue]engine.VenueSystemSettings, len(stored.Venues))
			for v, entry := range stored.Venues {
				if entry.Live.PrivateKey, err = s.secrets.Encrypt(entry.Live.PrivateKey); err != nil {
					return fmt.Errorf("加密 %s 实盘私钥: %w", v.Label(), err)
				}
				if entry.Live.APIKey, err = s.secrets.Encrypt(entry.Live.APIKey); err != nil {
					return fmt.Errorf("加密 %s 实盘 API Key: %w", v.Label(), err)
				}
				if entry.Live.APISecret, err = s.secrets.Encrypt(entry.Live.APISecret); err != nil {
					return fmt.Errorf("加密 %s L2 API Secret: %w", v.Label(), err)
				}
				if entry.Live.APIPassphrase, err = s.secrets.Encrypt(entry.Live.APIPassphrase); err != nil {
					return fmt.Errorf("加密 %s L2 API Passphrase: %w", v.Label(), err)
				}
				venues[v] = entry
			}
			stored.Venues = venues
		}
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO system_settings(id, payload, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		string(payload), formatTime(time.Now().UTC()))
	return err
}

// LoadAccessHash returns the bcrypt hash of the web access password, or an
// empty string when password protection is disabled.
func (s *SQLite) LoadAccessHash(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM access_password WHERE id = 1").Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

// SaveAccessHash stores a new hash, or removes the row when hash is empty so
// password protection can be turned off again.
func (s *SQLite) SaveAccessHash(ctx context.Context, hash string) error {
	if hash == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM access_password WHERE id = 1")
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO access_password(id, password_hash, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET password_hash=excluded.password_hash, updated_at=excluded.updated_at`,
		hash, formatTime(time.Now().UTC()))
	return err
}
