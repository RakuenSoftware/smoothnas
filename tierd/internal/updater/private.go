package updater

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"
)

type privateRepoInfo struct {
	Branch    string
	Version   string
	Published string
	Body      string
}

var (
	inspectPrivateRepoForUser = inspectPrivateRepoSSH
	applyPrivateRepoForUser   = applyPrivateRepoSSH
	lookupSystemUser          = sgauth.GetUser
)

func inspectPrivateRepoSSH(username string) (*privateRepoInfo, error) {
	cloneDir, branch, err := clonePrivateRepo(username)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(cloneDir)

	return describePrivateRepo(cloneDir, branch)
}

func applyPrivateRepoSSH(u *Updater, username string) error {
	cloneDir, branch, err := clonePrivateRepo(username)
	if err != nil {
		return err
	}
	defer os.RemoveAll(cloneDir)

	info, err := describePrivateRepo(cloneDir, branch)
	if err != nil {
		return err
	}

	u.setStage("building")
	binaryStagePath, uiStageDir, err := buildPrivateRepo(cloneDir, info.Version)
	if err != nil {
		return err
	}

	u.setStage("installing")
	if err := backupAndReplace(binaryStagePath, binaryPath, 0o755); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	if err := replaceUIDir(uiStageDir, uiPath); err != nil {
		return fmt.Errorf("replace UI: %w", err)
	}

	EnsureSystemPackages()
	os.RemoveAll(privateBuildRoot)

	u.mu.Lock()
	u.cachedStatus = nil
	u.mu.Unlock()

	u.setStage("restarting")
	time.Sleep(4 * time.Second)
	exec.Command("systemctl", "restart", "tierd.service").Start()

	return nil
}

func clonePrivateRepo(username string) (string, string, error) {
	branch, err := privateRepoDefaultBranch(username)
	if err != nil {
		return "", "", err
	}

	cloneDir, err := os.MkdirTemp("", "smoothnas-private-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp clone dir: %w", err)
	}

	cmd, err := gitCommandForUser(username, "clone", "--depth", "1", "--branch", branch, privateRepoSSH, cloneDir)
	if err != nil {
		os.RemoveAll(cloneDir)
		return "", "", err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(cloneDir)
		return "", "", fmt.Errorf("clone %s/%s: %s: %w", privateOwner, privateRepo, strings.TrimSpace(string(out)), err)
	}

	return cloneDir, branch, nil
}

func privateRepoDefaultBranch(username string) (string, error) {
	out, err := runUserGit(username, "ls-remote", "--symref", privateRepoSSH, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") && strings.HasSuffix(line, "\tHEAD") {
			branch := strings.TrimSuffix(strings.TrimPrefix(line, "ref: refs/heads/"), "\tHEAD")
			if branch != "" {
				return branch, nil
			}
		}
	}
	return "", fmt.Errorf("could not determine default branch for %s/%s", privateOwner, privateRepo)
}

func describePrivateRepo(dir, branch string) (*privateRepoInfo, error) {
	cmd := exec.Command("git", "-C", dir, "log", "-1", "--format=%ct%n%h%n%s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect private repo: %s: %w", strings.TrimSpace(string(out)), err)
	}

	parts := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(parts) < 3 {
		return nil, fmt.Errorf("unexpected private repo metadata output")
	}

	epoch, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse private repo commit time: %w", err)
	}

	shortSHA := strings.TrimSpace(parts[1])
	subject := strings.TrimSpace(parts[2])
	ts := time.Unix(epoch, 0).UTC()

	return &privateRepoInfo{
		Branch:    branch,
		Version:   ts.Format("2006.0102.1504") + "-" + shortSHA,
		Published: ts.Format(time.RFC3339),
		Body:      fmt.Sprintf("Repo: %s/%s\nBranch: %s\nCommit: %s\nSubject: %s", privateOwner, privateRepo, branch, shortSHA, subject),
	}, nil
}

func buildPrivateRepo(dir, version string) (string, string, error) {
	if err := os.RemoveAll(privateBuildRoot); err != nil {
		return "", "", fmt.Errorf("clean private build root: %w", err)
	}
	if err := os.MkdirAll(privateBuildRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("create private build root: %w", err)
	}

	binaryOut := filepath.Join(privateBuildRoot, "tierd")
	goCache := filepath.Join(privateBuildRoot, "go-build-cache")
	goTmp := filepath.Join(privateBuildRoot, "go-tmp")
	if err := os.MkdirAll(goCache, 0o755); err != nil {
		return "", "", fmt.Errorf("create go cache: %w", err)
	}
	if err := os.MkdirAll(goTmp, 0o755); err != nil {
		return "", "", fmt.Errorf("create go tmp: %w", err)
	}

	goCmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", binaryOut, "./cmd/tierd/")
	goCmd.Dir = filepath.Join(dir, "tierd")
	goCmd.Env = append(os.Environ(),
		"CGO_ENABLED=1",
		"GOCACHE="+goCache,
		"GOTMPDIR="+goTmp,
	)
	if out, err := goCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("build private backend: %s: %w", strings.TrimSpace(string(out)), err)
	}

	npmCmd := exec.Command("npm", "ci")
	npmCmd.Dir = filepath.Join(dir, "tierd-ui")
	if out, err := npmCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("install private frontend deps: %s: %w", strings.TrimSpace(string(out)), err)
	}

	ngCmd := exec.Command("npx", "ng", "build", "--output-path=../dist/tierd-ui")
	ngCmd.Dir = filepath.Join(dir, "tierd-ui")
	if out, err := ngCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("build private frontend: %s: %w", strings.TrimSpace(string(out)), err)
	}

	uiDir := filepath.Join(dir, "dist", "tierd-ui", "browser")
	if _, err := os.Stat(uiDir); err != nil {
		return "", "", fmt.Errorf("private frontend output missing: %w", err)
	}

	return binaryOut, uiDir, nil
}

func runUserGit(username string, args ...string) (string, error) {
	cmd, err := gitCommandForUser(username, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

func gitCommandForUser(username string, args ...string) (*exec.Cmd, error) {
	if username == "" {
		return nil, fmt.Errorf("authenticated username required")
	}

	user, err := lookupSystemUser(username)
	if err != nil {
		return nil, fmt.Errorf("lookup user %s: %w", username, err)
	}

	uid, err := strconv.ParseUint(user.UID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse uid for %s: %w", username, err)
	}
	gid, err := strconv.ParseUint(user.GID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse gid for %s: %w", username, err)
	}

	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"HOME="+user.Home,
		"USER="+user.Username,
		"LOGNAME="+user.Username,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
	return cmd, nil
}

func replaceUIDir(srcDir, destDir string) error {
	bakDir := destDir + ".bak"
	if err := os.RemoveAll(bakDir); err != nil {
		return fmt.Errorf("remove old UI backup: %w", err)
	}
	if _, err := os.Stat(destDir); err == nil {
		if err := os.Rename(destDir, bakDir); err != nil {
			return fmt.Errorf("backup UI: %w", err)
		}
	}

	tmpDir := destDir + ".new"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clean new UI dir: %w", err)
	}
	if err := copyDir(srcDir, tmpDir); err != nil {
		return err
	}

	return os.Rename(tmpDir, destDir)
}

func copyDir(srcDir, destDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		dstFile, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		if _, err := io.Copy(dstFile, srcFile); err != nil {
			dstFile.Close()
			return err
		}
		return dstFile.Close()
	})
}
