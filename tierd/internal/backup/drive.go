package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DriveConfig is populated from the selected account immediately before a run.
type DriveConfig struct {
	ClientID, ClientSecret, RefreshToken string
	Encrypt                              bool
	CryptPassword, CryptSalt, Excludes   string
}

func obscure(ctx context.Context, value string) (string, error) {
	cmd := exec.CommandContext(ctx, "rclone", "obscure", "-")
	cmd.Stdin = strings.NewReader(value + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rclone obscure: ensure rclone is installed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
func NewDriveKeys(ctx context.Context) (string, string, error) {
	key := make([]byte, 32)
	salt := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	password, err := obscure(ctx, base64.RawURLEncoding.EncodeToString(key))
	if err != nil {
		return "", "", err
	}
	obscuredSalt, err := obscure(ctx, base64.RawURLEncoding.EncodeToString(salt))
	return password, obscuredSalt, err
}
func DriveRecoveryKit(cfg Config) string {
	token, _ := json.Marshal(map[string]string{"refresh_token": cfg.Drive.RefreshToken, "token_type": "Bearer", "expiry": "0001-01-01T00:00:00Z"})
	return fmt.Sprintf(`# SmoothNAS recovery kit — keep private and store away from the NAS.
# Save this file as smoothnas-recovery.conf. Install rclone, then restore with:
# rclone --config ./smoothnas-recovery.conf copy sncrypt: /path/to/restore
# Reuse this OAuth client/project. A different project cannot see these files.

[sndrive]
type = drive
client_id = %s
client_secret = %s
scope = drive.file
token = %s

[sncrypt]
type = crypt
remote = sndrive:%s
password = %s
password2 = %s
filename_encryption = standard
directory_name_encryption = true
`, cfg.Drive.ClientID, cfg.Drive.ClientSecret, token, cfg.RemotePath, cfg.Drive.CryptPassword, cfg.Drive.CryptSalt)
}

func driveEnvironment(cfg Config) []string {
	// Ignore inherited rclone configuration, including debug/dump options.
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "RCLONE_") {
			env = append(env, v)
		}
	}
	token, _ := json.Marshal(map[string]string{"refresh_token": cfg.Drive.RefreshToken, "token_type": "Bearer", "expiry": "0001-01-01T00:00:00Z"})
	env = append(env, "RCLONE_CONFIG_SNDRIVE_TYPE=drive", "RCLONE_CONFIG_SNDRIVE_CLIENT_ID="+cfg.Drive.ClientID, "RCLONE_CONFIG_SNDRIVE_CLIENT_SECRET="+cfg.Drive.ClientSecret, "RCLONE_CONFIG_SNDRIVE_SCOPE=drive.file", "RCLONE_CONFIG_SNDRIVE_TOKEN="+string(token))
	if cfg.Drive.Encrypt {
		env = append(env, "RCLONE_CONFIG_SNCRYPT_TYPE=crypt", "RCLONE_CONFIG_SNCRYPT_REMOTE=sndrive:"+cfg.RemotePath, "RCLONE_CONFIG_SNCRYPT_PASSWORD="+cfg.Drive.CryptPassword, "RCLONE_CONFIG_SNCRYPT_PASSWORD2="+cfg.Drive.CryptSalt, "RCLONE_CONFIG_SNCRYPT_FILENAME_ENCRYPTION=standard", "RCLONE_CONFIG_SNCRYPT_DIRECTORY_NAME_ENCRYPTION=true")
	}
	return env
}
func driveArgs(cfg Config) []string {
	remote := "sndrive:" + cfg.RemotePath
	if cfg.Drive.Encrypt {
		remote = "sncrypt:"
	}
	action := "copy"
	src, dst := cfg.LocalPath, remote
	if cfg.Direction == "pull" {
		src, dst = remote, cfg.LocalPath
	} else if cfg.DeleteMode {
		action = "sync"
	}
	n := cfg.Parallelism
	if n < 1 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return []string{action, "--config", "/dev/null", "--transfers", strconv.Itoa(n), "--checkers", strconv.Itoa(2 * n), "--fast-list", "--drive-stop-on-upload-limit", "--use-json-log", "--stats", "5s", "--stats-log-level", "NOTICE", "--", src, dst}
}
func redactDrive(s string, cfg Config) string {
	for _, secret := range []string{cfg.Drive.ClientSecret, cfg.Drive.RefreshToken, cfg.Drive.CryptPassword, cfg.Drive.CryptSalt} {
		if secret != "" {
			encoded, _ := json.Marshal(secret)
			s = strings.ReplaceAll(s, string(encoded[1:len(encoded)-1]), "[REDACTED]")
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}

type driveLog struct {
	Msg   string `json:"msg"`
	Level string `json:"level"`
	Stats *struct {
		Bytes          int64   `json:"bytes"`
		TotalBytes     int64   `json:"totalBytes"`
		Transfers      int     `json:"transfers"`
		TotalTransfers int     `json:"totalTransfers"`
		Speed          float64 `json:"speed"`
	} `json:"stats"`
}

func runDrive(ctx context.Context, cfg Config, progress func(string, int, int)) (string, error) {
	args := driveArgs(cfg)
	if strings.TrimSpace(cfg.Drive.Excludes) != "" {
		f, err := os.CreateTemp("", "smoothnas-drive-excludes-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		for _, pattern := range strings.Split(cfg.Drive.Excludes, "\n") {
			pattern = strings.TrimSpace(pattern)
			if pattern != "" {
				if _, err = f.WriteString(pattern + "\n"); err != nil {
					f.Close()
					return "", err
				}
			}
		}
		if err = f.Close(); err != nil {
			return "", err
		}
		// --exclude-from reads patterns, not filter rules; exclusions must never
		// accidentally become include rules.
		args = append(args[:1], append([]string{"--exclude-from", f.Name()}, args[1:]...)...)
	}
	cmd := ioprioIdleCmd(ctx, "rclone", args...)
	cmd.Env = driveEnvironment(cfg)
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = io.Discard
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("start rclone: %w", err)
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var tail bytes.Buffer
	quota := false
	otherError := false
	for scanner.Scan() {
		line := redactDrive(scanner.Text(), cfg)
		var entry driveLog
		if json.Unmarshal([]byte(line), &entry) == nil {
			if st := entry.Stats; st != nil && progress != nil {
				progress(fmt.Sprintf("rclone: %.2f MB/s (%d / %d bytes)", st.Speed/1e6, st.Bytes, st.TotalBytes), st.Transfers, st.TotalTransfers)
			}
			daily := dailyUploadLimit(entry.Msg)
			if daily && strings.Contains(strings.ToLower(entry.Msg), "received upload limit error") {
				quota = true
			}
			if strings.EqualFold(entry.Level, "error") && !daily {
				lower := strings.ToLower(entry.Msg)
				if !strings.Contains(lower, "context canceled") && !strings.Contains(lower, "fatal error received") {
					otherError = true
				}
			}
		}
		if tail.Len() > 16000 {
			tail.Reset()
		}
		tail.WriteString(line + "\n")
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = cmd.Process.Kill()
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if scanErr != nil {
		return "", fmt.Errorf("read rclone progress: %w", scanErr)
	}
	if err != nil {
		if quota && !otherError {
			return "Stopped at Google's daily upload limit — the next scheduled run continues.", nil
		}
		return "", fmt.Errorf("rclone: %w: %s", err, redactDrive(tail.String(), cfg))
	}
	return "Google Drive backup completed", nil
}

// rclone also calls storage exhaustion an upload limit. Only its daily-rate
// variant is recoverable on tomorrow's run; full storage remains a failure.
func dailyUploadLimit(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "user rate limit exceeded.") || strings.Contains(lower, "userratelimitexceeded") || strings.Contains(lower, "daily upload limit")
}
