package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("Usage: mygithelper <command>\nCommands:\n  get      Clone or pull all repos in all groups\n  update   Update GitHub Actions and create PRs\n  reset    Hard reset all repos (git reset --hard)")
	}

	baseDir, err := os.Getwd()
	if err != nil {
		fatalf("failed to get working directory: %v", err)
	}

	cfg := &Config{BaseDir: baseDir}
	if err := cfg.Load(); err != nil {
		fatalf("%v", err)
	}

	var cmdErr error
	switch os.Args[1] {
	case "get":
		cmdErr = (&GetCmd{Config: cfg}).Run()
	case "update":
		cmdErr = (&UpdateCmd{Config: cfg}).Run()
	case "reset":
		cmdErr = (&ResetCmd{Config: cfg}).Run()
	default:
		fatalf("Unknown command: %s", os.Args[1])
	}

	if cmdErr != nil {
		fatalf("%v", cmdErr)
	}
}

// Config holds the common configuration for all commands.
type Config struct {
	BaseDir string
	Groups  []string
}

func (c *Config) Load() error {
	groupsFile := filepath.Join(c.BaseDir, "myrepogroups.txt")
	groups, err := readLines(groupsFile)
	if err != nil {
		return fmt.Errorf("failed to read groups file %s: %w", groupsFile, err)
	}
	c.Groups = groups
	return nil
}

func (c *Config) ReposFile(group string) string {
	return filepath.Join(c.BaseDir, fmt.Sprintf("myrepogroups.%s.txt", group))
}

func (c *Config) GroupDir(group string) string {
	return filepath.Join(c.BaseDir, group)
}

// ForEachRepo iterates over all repos in all groups.
func (c *Config) ForEachRepo(fn func(group, repo, repoDir string) error) error {
	for _, group := range c.Groups {
		groupDir := c.GroupDir(group)
		repos, err := readLines(c.ReposFile(group))
		if err != nil {
			return fmt.Errorf("failed to read repos file: %w", err)
		}

		for _, repo := range repos {
			repoName := repoNameFromPath(repo)
			if repoName == "" {
				continue
			}
			repoDir := filepath.Join(groupDir, repoName)
			if err := fn(group, repo, repoDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- Get command ---

type GetCmd struct {
	*Config
}

func (cmd *GetCmd) Run() error {
	if len(cmd.Groups) == 0 {
		return fmt.Errorf("no groups found")
	}

	for _, group := range cmd.Groups {
		if err := cmd.processGroup(group); err != nil {
			return err
		}
	}
	return nil
}

func (cmd *GetCmd) processGroup(group string) error {
	groupDir := cmd.GroupDir(group)

	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return fmt.Errorf("failed to create group directory %s: %w", groupDir, err)
	}

	repos, err := readLines(cmd.ReposFile(group))
	if err != nil {
		return fmt.Errorf("failed to read repos file: %w", err)
	}

	if len(repos) == 0 {
		fmt.Printf("No repos found in group %q, skipping\n", group)
		return nil
	}

	// Build set of expected repo names
	expectedRepos := make(map[string]bool)
	for _, repo := range repos {
		if name := repoNameFromPath(repo); name != "" {
			expectedRepos[name] = true
		}
	}

	for _, repo := range repos {
		if err := cmd.processRepo(groupDir, repo); err != nil {
			return err
		}
	}

	// Remove repos that are no longer in the list
	return cmd.removeStaleRepos(groupDir, expectedRepos)
}

func (cmd *GetCmd) processRepo(groupDir, repo string) error {
	repoName := repoNameFromPath(repo)
	if repoName == "" {
		return fmt.Errorf("invalid repo format %q: expected owner/repo", repo)
	}
	repoDir := filepath.Join(groupDir, repoName)

	if dirExists(repoDir) {
		return cmd.pull(repoDir, repo)
	}
	return cmd.clone(groupDir, repo)
}

func (cmd *GetCmd) clone(groupDir, repo string) error {
	repoURL := fmt.Sprintf("git@github.com:%s.git", repo)
	fmt.Printf("Cloning %s...\n", repo)

	if err := gitRun(groupDir, "clone", repoURL); err != nil {
		return fmt.Errorf("failed to clone %s: %w\n\nPlease check:\n  - You have SSH access to GitHub (run: ssh -T git@github.com)\n  - The repository exists and you have access to it", repo, err)
	}
	return nil
}

func (cmd *GetCmd) pull(repoDir, repo string) error {
	if dirty, status, err := checkUncommitted(repoDir); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("repo %s has uncommitted changes:\n%s\nPlease commit or stash your changes before running get", repo, status)
	}

	fmt.Printf("Pulling %s...\n", repo)

	if err := gitRun(repoDir, "pull"); err != nil {
		return fmt.Errorf("failed to pull %s: %w\n\nPlease check:\n  - You are on a branch that tracks a remote branch\n  - There are no merge conflicts", repo, err)
	}
	return nil
}

func (cmd *GetCmd) removeStaleRepos(groupDir string, expectedRepos map[string]bool) error {
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		return fmt.Errorf("failed to read group directory %s: %w", groupDir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		repoName := entry.Name()
		if expectedRepos[repoName] {
			continue
		}

		repoDir := filepath.Join(groupDir, repoName)

		// Check if it's a git repo
		if !dirExists(filepath.Join(repoDir, ".git")) {
			continue
		}

		if dirty, status, err := checkUncommitted(repoDir); err != nil {
			return err
		} else if dirty {
			return fmt.Errorf("repo %s is no longer in the list but has uncommitted changes:\n%s\nPlease commit, stash, or manually remove the directory: %s", repoName, status, repoDir)
		}

		fmt.Printf("Removing %s (no longer in list)...\n", repoName)
		if err := os.RemoveAll(repoDir); err != nil {
			return fmt.Errorf("failed to remove %s: %w", repoDir, err)
		}
	}

	return nil
}

// --- Update command ---

type UpdateCmd struct {
	*Config
	GoVersion   string
	PrevVersion string
}

func (cmd *UpdateCmd) Run() error {
	// Check dependencies
	if _, err := exec.LookPath("ghat"); err != nil {
		return fmt.Errorf("ghat is required but not installed.\nInstall: go install github.com/JamesWoolfenden/ghat@latest")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh (GitHub CLI) is required but not installed.\nInstall: https://cli.github.com/")
	}

	// Parse Go version from this repo's go.mod
	goVersion, err := parseGoVersion(cmd.BaseDir)
	if err != nil {
		return fmt.Errorf("failed to parse Go version from go.mod: %w", err)
	}
	cmd.GoVersion = goVersion
	cmd.PrevVersion = prevGoVersion(goVersion)

	fmt.Printf("Using Go versions: %s.x (current), %s.x (previous)\n", cmd.GoVersion, cmd.PrevVersion)

	return cmd.ForEachRepo(func(group, repo, repoDir string) error {
		if !dirExists(repoDir) {
			fmt.Printf("Skipping %s: not cloned\n", repo)
			return nil
		}
		return cmd.updateRepo(repoDir, repo)
	})
}

func (cmd *UpdateCmd) updateRepo(repoDir, repo string) error {
	fmt.Printf("\n=== Updating %s ===\n", repo)

	// Check for uncommitted changes
	if dirty, status, err := checkUncommitted(repoDir); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("repo %s has uncommitted changes:\n%s\nPlease commit or stash your changes", repo, status)
	}

	// Get default branch and ensure we're on it
	defaultBranch, err := getDefaultBranch(repoDir)
	if err != nil {
		return fmt.Errorf("%s: failed to get default branch: %w", repo, err)
	}

	currentBranch, err := gitOutput(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("%s: failed to get current branch: %w", repo, err)
	}
	currentBranch = strings.TrimSpace(currentBranch)

	if currentBranch != defaultBranch {
		fmt.Printf("Switching to %s...\n", defaultBranch)
		if err := gitRun(repoDir, "checkout", defaultBranch); err != nil {
			return fmt.Errorf("%s: failed to checkout %s: %w", repo, defaultBranch, err)
		}
	}

	// Pull latest
	if err := gitRun(repoDir, "pull"); err != nil {
		return fmt.Errorf("%s: failed to pull: %w", repo, err)
	}

	// Run all update steps
	if err := cmd.runUpdateSteps(repoDir); err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}

	// Build commit message based on actual changes
	var updates []string
	if testYmlChanged(repoDir) {
		updates = append(updates, fmt.Sprintf("Go %s.x/%s.x, GitHub Actions", cmd.PrevVersion, cmd.GoVersion))
	}
	if goModChanged(repoDir) {
		updates = append(updates, fmt.Sprintf("go.mod Go %s, dependencies", cmd.PrevVersion))
	}

	if len(updates) == 0 {
		fmt.Println("No changes to commit")
		return nil
	}

	// Generate branch name from hash of all changed files
	branchName, err := cmd.generateBranchName(repoDir)
	if err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}

	// Check if branch already exists remotely
	if branchExistsRemote(repoDir, branchName) {
		fmt.Printf("Branch %s already exists, skipping\n", branchName)
		if err := gitRun(repoDir, "checkout", "."); err != nil {
			return fmt.Errorf("%s: failed to revert changes: %w", repo, err)
		}
		return nil
	}

	// Create branch, commit, push, and create PR
	commitMsg := "Update " + strings.Join(updates, ", ")
	prBody := "Updates: " + strings.Join(updates, ", ") + "\n\n---\nCreated by mygithelper"

	if err := cmd.createPR(repoDir, defaultBranch, branchName, commitMsg, prBody); err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}

	return nil
}

func (cmd *UpdateCmd) runUpdateSteps(repoDir string) error {
	// Step 1: Update test.yml with Go versions (optional - file may not exist)
	if hasTestYml(repoDir) {
		fmt.Println("Updating test.yml...")
		if _, _, err := cmd.updateTestYml(repoDir); err != nil {
			return fmt.Errorf("failed to update test.yml: %w", err)
		}
	}

	// Step 2: Run ghat on .github/workflows (optional - directory may not exist)
	if hasWorkflowsDir(repoDir) {
		fmt.Println("Running ghat swot...")
		if err := runGhat(repoDir); err != nil {
			return fmt.Errorf("ghat failed: %w", err)
		}
	}

	// Step 3: Update Go version in go.mod (optional - go.mod may not exist)
	if hasGoMod(repoDir) {
		fmt.Printf("Setting go.mod version to %s...\n", cmd.PrevVersion)
		if err := goRun(repoDir, "mod", "edit", "-go", cmd.PrevVersion); err != nil {
			return fmt.Errorf("go mod edit failed: %w", err)
		}
	}

	// Step 4: Update dependencies (optional - go.mod may not exist)
	if hasGoMod(repoDir) {
		fmt.Println("Updating dependencies...")
		if err := goRun(repoDir, "get", "-t", "-u", "./..."); err != nil {
			return fmt.Errorf("go get failed: %w", err)
		}
	}

	return nil
}

func (cmd *UpdateCmd) generateBranchName(repoDir string) (string, error) {
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

func (cmd *UpdateCmd) createPR(repoDir, defaultBranch, branchName, commitMsg, prBody string) error {
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

func (cmd *UpdateCmd) updateTestYml(repoDir string) (newContent []byte, updated bool, err error) {
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

// --- Reset command ---

type ResetCmd struct {
	*Config
}

func (cmd *ResetCmd) Run() error {
	return cmd.ForEachRepo(func(group, repo, repoDir string) error {
		if !dirExists(repoDir) {
			return nil
		}
		fmt.Printf("Resetting %s...\n", repo)
		if err := gitRun(repoDir, "reset", "--hard"); err != nil {
			return fmt.Errorf("%s: git reset --hard failed: %w", repo, err)
		}
		return nil
	})
}

// --- Helpers ---

func repoNameFromPath(repo string) string {
	parts := strings.Split(repo, "/")
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
	cmd := exec.Command("ghat", "swot", "-d", ".")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	cmd := exec.Command("gh", "pr", "create", "--title", title, "--body", body)
	cmd.Dir = repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
