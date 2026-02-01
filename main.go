package main

import (
	"bufio"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
)

type repo struct {
	Group    string
	Path     string
	Name     string
	Dir      string
	GroupDir string
}

func main() {
	if len(os.Args) < 2 {
		fatalf("Usage: mygithelper <command>\nCommands:\n  get      Clone all repos, or checkout default branch for existing\n  update   Update GitHub Actions and create PRs\n  reset    Hard reset, checkout default branch, and pull")
	}

	baseDir, err := os.Getwd()
	if err != nil {
		fatalf("failed to get working directory: %v", err)
	}

	cfg := &config{BaseDir: baseDir}
	if err := cfg.Load(); err != nil {
		fatalf("%v", err)
	}

	var cmdErr error
	switch os.Args[1] {
	case "get":
		cmdErr = (&getCmd{config: cfg}).Run()
	case "update":
		cmdErr = (&updateCmd{config: cfg}).Run()
	case "reset":
		cmdErr = (&resetCmd{config: cfg}).Run()
	default:
		fatalf("Unknown command: %s", os.Args[1])
	}

	if cmdErr != nil {
		fatalf("%v", cmdErr)
	}
}

type config struct {
	BaseDir string
	Groups  []string
}

func (c *config) Load() error {
	groupsFile := filepath.Join(c.BaseDir, "myrepogroups.txt")
	groups, err := readLines(groupsFile)
	if err != nil {
		return fmt.Errorf("failed to read groups file %s: %w", groupsFile, err)
	}
	c.Groups = groups
	return nil
}

func (c *config) ReposFile(group string) string {
	return filepath.Join(c.BaseDir, fmt.Sprintf("myrepogroups.%s.txt", group))
}

func (c *config) GroupDir(group string) string {
	return filepath.Join(c.BaseDir, group)
}

// Repos returns an iterator over all repos in all groups.
func (c *config) Repos() iter.Seq[repo] {
	return func(yield func(repo) bool) {
		for _, group := range c.Groups {
			for repo := range c.ReposInGroup(group) {
				if !yield(repo) {
					return
				}
			}
		}
	}
}

// ReposInGroup returns an iterator over repos in a specific group.
func (c *config) ReposInGroup(group string) iter.Seq[repo] {
	return func(yield func(repo) bool) {
		groupDir := c.GroupDir(group)
		repos, err := readLines(c.ReposFile(group))
		if err != nil {
			return
		}

		for _, repoPath := range repos {
			repoName := repoNameFromPath(repoPath)
			if repoName == "" {
				continue
			}
			r := repo{
				Group:    group,
				Path:     repoPath,
				Name:     repoName,
				Dir:      filepath.Join(groupDir, repoName),
				GroupDir: groupDir,
			}
			if !yield(r) {
				return
			}
		}
	}
}

// --- Get command ---

type getCmd struct {
	*config
}

func (cmd *getCmd) Run() error {
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

func (cmd *getCmd) processGroup(group string) error {
	groupDir := cmd.GroupDir(group)

	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return fmt.Errorf("failed to create group directory %s: %w", groupDir, err)
	}

	// Build set of expected repo names and process each repo
	expectedRepos := make(map[string]bool)
	repoCount := 0
	for repo := range cmd.ReposInGroup(group) {
		expectedRepos[repo.Name] = true
		repoCount++
		if err := cmd.processRepo(repo); err != nil {
			return err
		}
	}

	if repoCount == 0 {
		fmt.Printf("No repos found in group %q, skipping\n", group)
		return nil
	}

	// Remove repos that are no longer in the list
	return cmd.removeStaleRepos(groupDir, expectedRepos)
}

func (cmd *getCmd) processRepo(repo repo) error {
	if dirExists(repo.Dir) {
		return cmd.checkoutDefaultBranch(repo)
	}
	return cmd.clone(repo)
}

func (cmd *getCmd) clone(repo repo) error {
	repoURL := fmt.Sprintf("git@github.com:%s.git", repo.Path)
	fmt.Printf("Cloning %s...\n", repo.Path)

	if err := gitRun(repo.GroupDir, "clone", repoURL); err != nil {
		return fmt.Errorf("failed to clone %s: %w\n\nPlease check:\n  - You have SSH access to GitHub (run: ssh -T git@github.com)\n  - The repository exists and you have access to it", repo.Path, err)
	}
	return nil
}

func (cmd *getCmd) checkoutDefaultBranch(repo repo) error {
	if dirty, status, err := checkUncommitted(repo.Dir); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("repo %s has uncommitted changes:\n%s\nPlease commit or stash your changes before running get", repo.Path, status)
	}

	defaultBranch, err := getDefaultBranch(repo.Dir)
	if err != nil {
		return fmt.Errorf("%s: failed to get default branch: %w", repo.Path, err)
	}

	fmt.Printf("Checking out %s in %s...\n", defaultBranch, repo.Path)

	if err := gitRun(repo.Dir, "checkout", defaultBranch); err != nil {
		return fmt.Errorf("failed to checkout %s in %s: %w", defaultBranch, repo.Path, err)
	}
	return nil
}

func (cmd *getCmd) removeStaleRepos(groupDir string, expectedRepos map[string]bool) error {
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

type updateCmd struct {
	*config
	GoVersion   string
	PrevVersion string
}

func (cmd *updateCmd) Run() error {
	// Check dependencies (use shell to resolve aliases)
	if err := shellCommandExists("ghat"); err != nil {
		return fmt.Errorf("ghat is required but not installed.\nInstall: go install github.com/JamesWoolfenden/ghat@latest")
	}
	if err := shellCommandExists("gh"); err != nil {
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

	for repo := range cmd.Repos() {
		if !dirExists(repo.Dir) {
			fmt.Printf("Skipping %s: not cloned\n", repo.Path)
			continue
		}
		if err := cmd.updateRepo(repo); err != nil {
			return err
		}
	}
	return nil
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
	if err := cmd.runUpdateSteps(repo.Dir); err != nil {
		return fmt.Errorf("%s: %w", repo.Path, err)
	}

	// Build commit message based on actual changes
	var updates []string
	if testYmlChanged(repo.Dir) {
		updates = append(updates, fmt.Sprintf("Go %s.x/%s.x, GitHub Actions", cmd.PrevVersion, cmd.GoVersion))
	}
	if goModChanged(repo.Dir) {
		updates = append(updates, fmt.Sprintf("go.mod Go %s, dependencies", cmd.PrevVersion))
	}

	if len(updates) == 0 {
		fmt.Println("No changes to commit")
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

func (cmd *updateCmd) runUpdateSteps(repoDir string) error {
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

// --- Reset command ---

type resetCmd struct {
	*config
}

func (cmd *resetCmd) Run() error {
	for repo := range cmd.Repos() {
		if !dirExists(repo.Dir) {
			continue
		}
		fmt.Printf("Resetting %s...\n", repo.Path)
		if err := gitRun(repo.Dir, "reset", "--hard"); err != nil {
			return fmt.Errorf("%s: git reset --hard failed: %w", repo.Path, err)
		}

		// Checkout default branch
		defaultBranch, err := getDefaultBranch(repo.Dir)
		if err != nil {
			return fmt.Errorf("%s: failed to get default branch: %w", repo.Path, err)
		}
		if err := gitRun(repo.Dir, "checkout", defaultBranch); err != nil {
			return fmt.Errorf("%s: failed to checkout %s: %w", repo.Path, defaultBranch, err)
		}

		// Pull upstream
		if err := gitRun(repo.Dir, "pull"); err != nil {
			return fmt.Errorf("%s: git pull failed: %w", repo.Path, err)
		}
	}
	return nil
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
