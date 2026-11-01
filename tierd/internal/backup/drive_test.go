package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDriveArgumentsAndSecrets(t *testing.T) {
	for _, encrypt := range []bool{false, true} {
		for _, direction := range []string{"push", "pull"} {
			for _, deleteMode := range []bool{false, true} {
				cfg := Config{LocalPath: "/data", RemotePath: "backup", Direction: direction, DeleteMode: deleteMode, Drive: DriveConfig{Encrypt: encrypt, ClientSecret: "secret\"value", RefreshToken: "token-value", CryptPassword: "password-value", CryptSalt: "salt-value"}}
				args := driveArgs(cfg)
				want := "copy"
				if direction == "push" && deleteMode {
					want = "sync"
				}
				if args[0] != want {
					t.Fatal(args)
				}
				remote := "sndrive:backup"
				if encrypt {
					remote = "sncrypt:"
				}
				src, dst := "/data", remote
				if direction == "pull" {
					src, dst = remote, "/data"
				}
				if args[len(args)-2] != src || args[len(args)-1] != dst {
					t.Fatal(args)
				}
				for _, v := range []string{cfg.Drive.ClientSecret, cfg.Drive.RefreshToken, cfg.Drive.CryptPassword, cfg.Drive.CryptSalt} {
					if strings.Contains(strings.Join(args, " "), v) {
						t.Fatal("secret in command arguments")
					}
				}
				raw, _ := json.Marshal(map[string]string{"msg": cfg.Drive.ClientSecret + cfg.Drive.RefreshToken + cfg.Drive.CryptPassword + cfg.Drive.CryptSalt})
				redacted := redactDrive(string(raw), cfg)
				if strings.Contains(redacted, "value") || strings.Contains(redacted, "token-") {
					t.Fatal(redacted)
				}
				t.Setenv("RCLONE_DUMP", "auth")
				env := strings.Join(driveEnvironment(cfg), "\n")
				if strings.Contains(env, "RCLONE_DUMP=") {
					t.Fatal("inherited dump option")
				}
				if strings.Contains(env, "SNCRYPT_TYPE=crypt") != encrypt {
					t.Fatal("crypt environment")
				}
			}
		}
	}
}

// Exercise the production runner and stock rclone. Only the Drive backend is
// replaced by a local backend, so no Google credentials or external writes occur.
func TestDriveLocalRoundTripAndRecovery(t *testing.T) {
	binary, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone is required for local integration")
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			source, remote, restored := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(source, "video"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"document.txt": "important data\n", "video/large.txt": "excluded"} {
				if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			password, salt, err := NewDriveKeys(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wrapper := t.TempDir()
			script := "#!/bin/sh\nexport RCLONE_CONFIG_SNDRIVE_TYPE=local\nexec '" + binary + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(wrapper, "rclone"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := Config{LocalPath: source, RemotePath: remote, Direction: "push", Parallelism: 2, Drive: DriveConfig{Encrypt: encrypted, CryptPassword: password, CryptSalt: salt, Excludes: "/video/**"}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := runDrive(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			// An unchanged run must not rewrite the destination file.
			entries, err := os.ReadDir(remote)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected one file, got %d", len(entries))
			}
			before, err := entries[0].Info()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runDrive(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(filepath.Join(remote, entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("unchanged backup retransferred")
			}
			cfg.LocalPath = restored
			cfg.Direction = "pull"
			cfg.DeleteMode = true // restores always copy
			if _, err := runDrive(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(restored, "document.txt"))
			if err != nil || string(data) != "important data\n" {
				t.Fatalf("restore: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(restored, "video/large.txt")); !os.IsNotExist(err) {
				t.Fatal("exclude was not respected")
			}
			if encrypted {
				kit := strings.Replace(DriveRecoveryKit(cfg), "type = drive", "type = local", 1)
				configPath := filepath.Join(t.TempDir(), "rclone.conf")
				if err := os.WriteFile(configPath, []byte(kit), 0600); err != nil {
					t.Fatal(err)
				}
				fresh := t.TempDir()
				cmd := exec.CommandContext(ctx, binary, "--config", configPath, "copy", "sncrypt:", fresh)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("independent recovery: %v %s", err, out)
				}
				data, err := os.ReadFile(filepath.Join(fresh, "document.txt"))
				if err != nil || string(data) != "important data\n" {
					t.Fatalf("recovery kit: %q %v", data, err)
				}
			}
		})
	}
}
func TestDailyUploadLimitClassification(t *testing.T) {
	if !dailyUploadLimit("Received upload limit error: googleapi: Error 403: User rate limit exceeded., userRateLimitExceeded") {
		t.Fatal("daily limit not recognized")
	}
	if dailyUploadLimit("Received upload limit error: googleapi: Error 403: quotaExceeded") {
		t.Fatal("storage full treated as daily limit")
	}
}

func TestDriveProgressQuotaAndErrorRedaction(t *testing.T) {
	for _, scenario := range []string{"daily", "full", "other"} {
		t.Run(scenario, func(t *testing.T) {
			wrapper := t.TempDir()
			logs := []map[string]any{{"level": "notice", "msg": "stats", "stats": map[string]any{"bytes": 32, "totalBytes": 64, "transfers": 1, "totalTransfers": 2, "speed": 1000000}}}
			if scenario == "full" {
				logs = append(logs, map[string]any{"level": "error", "msg": "Received upload limit error: quotaExceeded token-value"})
			} else {
				logs = append(logs, map[string]any{"level": "error", "msg": "Received upload limit error: User rate limit exceeded., userRateLimitExceeded token-value"})
				if scenario == "other" {
					logs = append(logs, map[string]any{"level": "error", "msg": "Failed to copy: permission denied token-value"})
				}
			}
			var script strings.Builder
			script.WriteString("#!/bin/sh\ncat >&2 <<'SMOOTHNAS_TEST_LOGS'\n")
			for _, entry := range logs {
				raw, _ := json.Marshal(entry)
				script.Write(raw)
				script.WriteByte('\n')
			}
			script.WriteString("SMOOTHNAS_TEST_LOGS\nexit 1\n")
			if err := os.WriteFile(filepath.Join(wrapper, "rclone"), []byte(script.String()), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
			progress := ""
			done, total := 0, 0
			summary, err := runDrive(context.Background(), Config{LocalPath: "/data", RemotePath: "backup", Direction: "push", Drive: DriveConfig{RefreshToken: "token-value"}}, func(msg string, d, n int) { progress = msg; done = d; total = n })
			if !strings.Contains(progress, "1.00 MB/s") || done != 1 || total != 2 {
				t.Fatalf("progress=%q %d/%d", progress, done, total)
			}
			if scenario == "daily" {
				if err != nil || !strings.Contains(summary, "daily upload limit") {
					t.Fatalf("daily: %q %v", summary, err)
				}
			} else {
				if err == nil {
					t.Fatal("failure swallowed")
				}
				if strings.Contains(err.Error(), "token-value") {
					t.Fatal("token leaked")
				}
			}
		})
	}
}
