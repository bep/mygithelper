package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
)

type repo struct {
	Path string // GitHub path (e.g., "bep/firstupdotenv")
	Name string // Extracted repo name (e.g., "firstupdotenv")
	Dir  string // Full path on disk
}

func main() {
	if len(os.Args) < 2 {
		fatalf("Usage: mygithelper update [--force]\n\nWalks the directory tree and updates all repos found in gitjoin.txt files.\n\nFlags:\n  --force  Create PR even with only 1 update (default requires at least 2)")
	}

	baseDir, err := os.Getwd()
	if err != nil {
		fatalf("failed to get working directory: %v", err)
	}

	switch os.Args[1] {
	case "update":
		force := len(os.Args) > 2 && os.Args[2] == "--force"
		if err := (&updateCmd{BaseDir: baseDir, Force: force}).Run(); err != nil {
			fatalf("%v", err)
		}
	default:
		fatalf("Unknown command: %s", os.Args[1])
	}
}

// --- Update command ---

type updateCmd struct {
	BaseDir     string
	GoVersion   string
	PrevVersion string
	Force       bool
}

func (cmd *updateCmd) Run() error {
	// Check dependencies (use shell to resolve aliases)
	if err := shellCommandExists("ghat"); err != nil {
		return fmt.Errorf("ghat is required but not installed.\nInstall: go install github.com/JamesWoolfenden/ghat@latest")
	}
	if err := shellCommandExists("gh"); err != nil {
		return fmt.Errorf("gh (GitHub CLI) is required but not installed.\nInstall: https://cli.github.com/")
	}

	// Parse Go version from this repo's go.mod (optional)
	if goVersion, err := parseGoVersion(cmd.BaseDir); err == nil {
		cmd.GoVersion = goVersion
		cmd.PrevVersion = prevGoVersion(goVersion)
		fmt.Printf("Using Go versions: %s.x (current), %s.x (previous)\n", cmd.GoVersion, cmd.PrevVersion)
	} else {
		fmt.Println("No go.mod found in base directory, skipping Go version updates")
	}

	// Find and process all gitjoin.txt files
	repos, err := cmd.findRepos()
	if err != nil {
		return err
	}

	if len(repos) == 0 {
		fmt.Println("No repos found in gitjoin.txt files")
		return nil
	}

	fmt.Printf("Found %d repos in gitjoin.txt files\n", len(repos))

	for _, repo := range repos {
		if err := cmd.updateRepo(repo); err != nil {
			return err
		}
	}
	return nil
}

func (cmd *updateCmd) findRepos() ([]repo, error) {
	var repos []repo

	err := filepath.WalkDir(cmd.BaseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip .git directories
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}

		if d.Name() != "gitjoin.txt" {
			return nil
		}

		// Found a gitjoin.txt file
		gitjoinDir := filepath.Dir(path)
		lines, err := readLines(path)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}

		for _, line := range lines {
			repoPath := repoPathFromGitjoinLine(line)
			if repoPath == "" {
				continue
			}
			repoName := repoNameFromPath(repoPath)
			if repoName == "" {
				continue
			}
			repoDir := filepath.Join(gitjoinDir, repoName)
			if !dirExists(repoDir) {
				fmt.Printf("Skipping %s: not cloned at %s\n", repoPath, repoDir)
				continue
			}
			repos = append(repos, repo{
				Path: repoPath,
				Name: repoName,
				Dir:  repoDir,
			})
		}

		return nil
	})

	return repos, err
}

func (cmd *updateCmd) updateRepo(repo repo) error {
	fmt.Printf("\n=== Updating %s ===\n", repo.Path)

	// Check for uncommitted changes
	if dirty, status, err := checkUncommitted(repo.Dir); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("repo %s has uncommitted changes:\n%s\nPlease commit or stash your changes", repo.Path, status)
	}

	// Get default branch and ensure we're on it
	defaultBranch, err := getDefaultBranch(repo.Dir)
	if err != nil {
		return fmt.Errorf("%s: failed to get default branch: %w", repo.Path, err)
	}

	currentBranch, err := gitOutput(repo.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("%s: failed to get current branch: %w", repo.Path, err)
	}
	currentBranch = strings.TrimSpace(currentBranch)

	if currentBranch != defaultBranch {
		fmt.Printf("Switching to %s...\n", defaultBranch)
		if err := gitRun(repo.Dir, "checkout", defaultBranch); err != nil {
			return fmt.Errorf("%s: failed to checkout %s: %w", repo.Path, defaultBranch, err)
		}
	}

	// Pull latest
	if err := gitRun(repo.Dir, "pull"); err != nil {
		return fmt.Errorf("%s: failed to pull: %w", repo.Path, err)
	}

	// Run all update steps
	result, err := cmd.runUpdateSteps(repo.Dir)
	if err != nil {
		return fmt.Errorf("%s: %w", repo.Path, err)
	}

	// Build commit message based on what was actually updated
	var updates []string
	if result.UpdatedGoVersions && testYmlChanged(repo.Dir) {
		updates = append(updates, fmt.Sprintf("Go %s.x/%s.x", cmd.PrevVersion, cmd.GoVersion))
	}
	if result.UpdatedGitHubActions && testYmlChanged(repo.Dir) {
		updates = append(updates, "GitHub Actions")
	}
	if result.UpdatedGoMod && goModChanged(repo.Dir) {
		updates = append(updates, fmt.Sprintf("go.mod Go %s, dependencies", cmd.PrevVersion))
	}

	if len(updates) == 0 {
		fmt.Println("No changes to commit")
		return nil
	}

	// Require at least 2 updates unless --force is used
	if len(updates) < 2 && !cmd.Force {
		fmt.Printf("Only %d update(s), skipping PR (use --force to override)\n", len(updates))
		if err := gitRun(repo.Dir, "checkout", "."); err != nil {
			return fmt.Errorf("%s: failed to revert changes: %w", repo.Path, err)
		}
		return nil
	}

	// Generate branch name from hash of all changed files
	branchName, err := cmd.generateBranchName(repo.Dir)
	if err != nil {
		return fmt.Errorf("%s: %w", repo.Path, err)
	}

	// Check if branch already exists remotely
	if branchExistsRemote(repo.Dir, branchName) {
		fmt.Printf("Branch %s already exists, skipping\n", branchName)
		if err := gitRun(repo.Dir, "checkout", "."); err != nil {
			return fmt.Errorf("%s: failed to revert changes: %w", repo.Path, err)
		}
		return nil
	}

	// Create branch, commit, push, and create PR
	commitMsg := "Update " + strings.Join(updates, ", ")
	prBody := "Updates: " + strings.Join(updates, ", ") + "\n\n---\nCreated by mygithelper"

	if err := cmd.createPR(repo.Dir, defaultBranch, branchName, commitMsg, prBody); err != nil {
		return fmt.Errorf("%s: %w", repo.Path, err)
	}

	return nil
}

type updateResult struct {
	UpdatedGoVersions    bool
	UpdatedGitHubActions bool
	UpdatedGoMod         bool
}

func (cmd *updateCmd) runUpdateSteps(repoDir string) (updateResult, error) {
	var result updateResult

	// Step 1: Update test.yml with Go versions (optional - requires file and Go version config)
	if cmd.GoVersion != "" && hasTestYml(repoDir) {
		fmt.Println("Updating test.yml...")
		if _, _, err := cmd.updateTestYml(repoDir); err != nil {
			return result, fmt.Errorf("failed to update test.yml: %w", err)
		}
		result.UpdatedGoVersions = true
	}

	// Step 2: Run ghat on .github/workflows (optional - directory may not exist)
	if hasWorkflowsDir(repoDir) {
		fmt.Println("Running ghat swot...")
		if err := runGhat(repoDir); err != nil {
			return result, fmt.Errorf("ghat failed: %w", err)
		}
		result.UpdatedGitHubActions = true
	}

	// Step 3: Update Go version in go.mod (optional - requires go.mod and Go version config)
	if cmd.GoVersion != "" && hasGoMod(repoDir) {
		fmt.Printf("Setting go.mod version to %s...\n", cmd.PrevVersion)
		if err := goRun(repoDir, "mod", "edit", "-go", cmd.PrevVersion); err != nil {
			return result, fmt.Errorf("go mod edit failed: %w", err)
		}
		result.UpdatedGoMod = true
	}

	// Step 4: Update dependencies (optional - requires go.mod and Go version config)
	if cmd.GoVersion != "" && hasGoMod(repoDir) {
		fmt.Println("Updating dependencies...")
		if err := goRun(repoDir, "get", "-t", "-u", "./..."); err != nil {
			return result, fmt.Errorf("go get failed: %w", err)
		}
	}

	return result, nil
}

func (cmd *updateCmd) generateBranchName(repoDir string) (string, error) {
	h := xxhash.New()

	// Hash test.yml if changed
	if testYmlChanged(repoDir) {
		content, err := os.ReadFile(filepath.Join(repoDir, ".github", "workflows", "test.yml"))
		if err != nil {
			return "", err
		}
		h.Write(content)
	}

	// Hash go.mod if changed
	if goModChanged(repoDir) {
		content, err := os.ReadFile(filepath.Join(repoDir, "go.mod"))
		if err != nil {
			return "", err
		}
		h.Write(content)
	}

	return fmt.Sprintf("mygithelper/update-%x", h.Sum64()), nil
}

func (cmd *updateCmd) createPR(repoDir, defaultBranch, branchName, commitMsg, prBody string) error {
	if err := gitRun(repoDir, "checkout", "-b", branchName); err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}

	if err := gitRun(repoDir, "add", "-A"); err != nil {
		return fmt.Errorf("failed to stage changes: %w", err)
	}

	if err := gitRun(repoDir, "commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	fmt.Printf("Pushing branch %s...\n", branchName)
	if err := gitRun(repoDir, "push", "-u", "origin", branchName); err != nil {
		return fmt.Errorf("failed to push: %w", err)
	}

	fmt.Println("Creating PR...")
	if err := createPR(repoDir, commitMsg, prBody); err != nil {
		return fmt.Errorf("failed to create PR: %w", err)
	}

	if err := gitRun(repoDir, "checkout", defaultBranch); err != nil {
		return fmt.Errorf("failed to checkout %s: %w", defaultBranch, err)
	}

	return nil
}

func (cmd *updateCmd) updateTestYml(repoDir string) (newContent []byte, updated bool, err error) {
	testYmlPath := filepath.Join(repoDir, ".github", "workflows", "test.yml")
	content, err := os.ReadFile(testYmlPath)
	if err != nil {
		return nil, false, fmt.Errorf("no .github/workflows/test.yml found")
	}

	original := string(content)

	re := regexp.MustCompile(`(?m)(go-version:\s*)\[([^\]]*)\]`)
	newVersions := fmt.Sprintf("[%s.x, %s.x]", cmd.PrevVersion, cmd.GoVersion)
	result := re.ReplaceAllString(original, "${1}"+newVersions)

	if result == original {
		return nil, false, nil
	}

	newContent = []byte(result)
	if err := os.WriteFile(testYmlPath, newContent, 0o644); err != nil {
		return nil, false, fmt.Errorf("failed to write test.yml: %w", err)
	}

	return newContent, true, nil
}

// --- Helpers ---

// repoPathFromGitjoinLine extracts the GitHub repo path from a gitjoin.txt line.
// Input: "github.com/bep/firstupdotenv" -> Output: "bep/firstupdotenv"
func repoPathFromGitjoinLine(line string) string {
	line = strings.TrimPrefix(line, "github.com/")
	parts := strings.Split(line, "/")
	if len(parts) != 2 {
		return ""
	}
	return line
}

func repoNameFromPath(repoPath string) string {
	parts := strings.Split(repoPath, "/")
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func getDefaultBranch(repoDir string) (string, error) {
	output, err := gitOutput(repoDir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		branch := strings.TrimSpace(output)
		branch = strings.TrimPrefix(branch, "refs/remotes/origin/")
		return branch, nil
	}

	if _, err := gitOutput(repoDir, "rev-parse", "--verify", "main"); err == nil {
		return "main", nil
	}
	if _, err := gitOutput(repoDir, "rev-parse", "--verify", "master"); err == nil {
		return "master", nil
	}

	return "main", nil
}

func parseGoVersion(repoDir string) (string, error) {
	goModPath := filepath.Join(repoDir, "go.mod")
	content, err := os.ReadFile(goModPath)
	if err != nil {
		return "", fmt.Errorf("no go.mod found")
	}

	re := regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	matches := re.FindSubmatch(content)
	if matches == nil {
		return "", fmt.Errorf("no go version found in go.mod")
	}

	return string(matches[1]), nil
}

func prevGoVersion(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) != 2 {
		return version
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor <= 0 {
		return version
	}

	return fmt.Sprintf("%s.%d", parts[0], minor-1)
}

func branchExistsRemote(repoDir, branch string) bool {
	output, err := gitOutput(repoDir, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) != ""
}

func runGhat(repoDir string) error {
	return shellRun(repoDir, "ghat swot -d .")
}

func goRun(dir string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func goModChanged(repoDir string) bool {
	output, err := gitOutput(repoDir, "status", "--porcelain", "go.mod", "go.sum")
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) != ""
}

func testYmlChanged(repoDir string) bool {
	output, err := gitOutput(repoDir, "status", "--porcelain", ".github/workflows/test.yml")
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) != ""
}

func createPR(repoDir, title, body string) error {
	// Escape single quotes in title and body for shell
	escapedTitle := strings.ReplaceAll(title, "'", "'\"'\"'")
	escapedBody := strings.ReplaceAll(body, "'", "'\"'\"'")
	return shellRun(repoDir, fmt.Sprintf("gh pr create --title '%s' --body '%s'", escapedTitle, escapedBody))
}

// --- Git helpers ---

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	return string(output), err
}

func checkUncommitted(repoDir string) (dirty bool, status string, err error) {
	status, err = gitOutput(repoDir, "status", "--porcelain")
	if err != nil {
		return false, "", fmt.Errorf("failed to check git status in %s: %w", repoDir, err)
	}
	return len(status) > 0, status, nil
}

func readLines(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func hasTestYml(repoDir string) bool {
	return fileExists(filepath.Join(repoDir, ".github", "workflows", "test.yml"))
}

func hasWorkflowsDir(repoDir string) bool {
	return dirExists(filepath.Join(repoDir, ".github", "workflows"))
}

func hasGoMod(repoDir string) bool {
	return fileExists(filepath.Join(repoDir, "go.mod"))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// --- Shell helpers (for alias support) ---

func getShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "bash"
}

func shellCommandExists(command string) error {
	shell := getShell()
	cmd := exec.Command(shell, "-ic", "command -v "+command)
	return cmd.Run()
}

func shellRun(dir, command string) error {
	shell := getShell()
	cmd := exec.Command(shell, "-ic", command)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
