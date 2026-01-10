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

	switch os.Args[1] {
	case "get":
		if err := runGet(); err != nil {
			fatalf("%v", err)
		}
	case "update":
		if err := runUpdate(); err != nil {
			fatalf("%v", err)
		}
	case "reset":
		if err := runReset(); err != nil {
			fatalf("%v", err)
		}
	default:
		fatalf("Unknown command: %s", os.Args[1])
	}
}

func runGet() error {
	baseDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	groupsFile := filepath.Join(baseDir, "myrepogroups.txt")
	groups, err := readLines(groupsFile)
	if err != nil {
		return fmt.Errorf("failed to read groups file %s: %w", groupsFile, err)
	}

	if len(groups) == 0 {
		return fmt.Errorf("no groups found in %s", groupsFile)
	}

	for _, group := range groups {
		if err := processGroup(baseDir, group); err != nil {
			return err
		}
	}

	return nil
}

func processGroup(baseDir, group string) error {
	groupDir := filepath.Join(baseDir, group)

	if err := os.MkdirAll(groupDir, 0755); err != nil {
		return fmt.Errorf("failed to create group directory %s: %w", groupDir, err)
	}

	reposFile := filepath.Join(baseDir, fmt.Sprintf("myrepogroups.%s.txt", group))
	repos, err := readLines(reposFile)
	if err != nil {
		return fmt.Errorf("failed to read repos file %s: %w", reposFile, err)
	}

	if len(repos) == 0 {
		fmt.Printf("No repos found in group %q, skipping\n", group)
		return nil
	}

	// Build set of expected repo names
	expectedRepos := make(map[string]bool)
	for _, repo := range repos {
		parts := strings.Split(repo, "/")
		if len(parts) == 2 {
			expectedRepos[parts[1]] = true
		}
	}

	for _, repo := range repos {
		if err := processRepo(groupDir, repo); err != nil {
			return err
		}
	}

	// Remove repos that are no longer in the list
	if err := removeStaleRepos(groupDir, expectedRepos); err != nil {
		return err
	}

	return nil
}

func processRepo(groupDir, repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repo format %q: expected owner/repo", repo)
	}
	repoName := parts[1]
	repoDir := filepath.Join(groupDir, repoName)

	if dirExists(repoDir) {
		return pullRepo(repoDir, repo)
	}
	return cloneRepo(groupDir, repo)
}

func cloneRepo(groupDir, repo string) error {
	repoURL := fmt.Sprintf("git@github.com:%s.git", repo)
	fmt.Printf("Cloning %s...\n", repo)

	if err := gitRun(groupDir, "clone", repoURL); err != nil {
		return fmt.Errorf("failed to clone %s: %w\n\nPlease check:\n  - You have SSH access to GitHub (run: ssh -T git@github.com)\n  - The repository exists and you have access to it", repo, err)
	}
	return nil
}

func pullRepo(repoDir, repo string) error {
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

func removeStaleRepos(groupDir string, expectedRepos map[string]bool) error {
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

func runUpdate() error {
	// Check dependencies
	if _, err := exec.LookPath("ghat"); err != nil {
		return fmt.Errorf("ghat is required but not installed.\nInstall: go install github.com/JamesWoolfenden/ghat@latest")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh (GitHub CLI) is required but not installed.\nInstall: https://cli.github.com/")
	}

	baseDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// Parse Go version from this repo's go.mod (not the target repos)
	goVersion, err := parseGoVersion(baseDir)
	if err != nil {
		return fmt.Errorf("failed to parse Go version from go.mod: %w", err)
	}
	prevVersion := prevGoVersion(goVersion)
	fmt.Printf("Using Go versions: %s.x (current), %s.x (previous)\n", goVersion, prevVersion)

	groupsFile := filepath.Join(baseDir, "myrepogroups.txt")
	groups, err := readLines(groupsFile)
	if err != nil {
		return fmt.Errorf("failed to read groups file %s: %w", groupsFile, err)
	}

	for _, group := range groups {
		if err := updateGroup(baseDir, group, goVersion, prevVersion); err != nil {
			return err
		}
	}

	return nil
}

func updateGroup(baseDir, group, goVersion, prevVersion string) error {
	groupDir := filepath.Join(baseDir, group)
	reposFile := filepath.Join(baseDir, fmt.Sprintf("myrepogroups.%s.txt", group))

	repos, err := readLines(reposFile)
	if err != nil {
		return fmt.Errorf("failed to read repos file %s: %w", reposFile, err)
	}

	for _, repo := range repos {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 {
			continue
		}
		repoName := parts[1]
		repoDir := filepath.Join(groupDir, repoName)

		if !dirExists(repoDir) {
			fmt.Printf("Skipping %s: not cloned\n", repo)
			continue
		}

		if err := updateRepo(repoDir, repo, goVersion, prevVersion); err != nil {
			return err
		}
	}

	return nil
}

func updateRepo(repoDir, repo, goVersion, prevVersion string) error {
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

	// Track what was updated for commit message
	var updates []string

	// Update test.yml with Go versions
	testYmlContent, testYmlUpdated, err := updateTestYml(repoDir, goVersion, prevVersion)
	if err != nil {
		fmt.Printf("Warning: %v\n", err)
	} else if testYmlUpdated {
		updates = append(updates, fmt.Sprintf("Go test versions: %s.x, %s.x", prevVersion, goVersion))
	}

	// If test.yml wasn't updated, nothing to do (ghat runs on the test.yml we modify)
	if !testYmlUpdated {
		fmt.Println("No changes to test.yml, skipping")
		return nil
	}

	// Generate branch name from hash of test.yml content
	branchName := fmt.Sprintf("mygithelper/update-%x", xxhash.Sum64(testYmlContent))

	// Check if branch already exists remotely - if so, skip this repo
	if branchExistsRemote(repoDir, branchName) {
		fmt.Printf("Branch %s already exists, skipping\n", branchName)
		// Revert changes
		if err := gitRun(repoDir, "checkout", "."); err != nil {
			return fmt.Errorf("%s: failed to revert changes: %w", repo, err)
		}
		return nil
	}

	// Run ghat
	fmt.Println("Running ghat swot...")
	if err := runGhat(repoDir); err != nil {
		fmt.Printf("Warning: ghat failed: %v\n", err)
	} else {
		updates = append(updates, "GitHub Actions: updated to pinned hashes")
	}

	// Create and checkout new branch
	if err := gitRun(repoDir, "checkout", "-b", branchName); err != nil {
		return fmt.Errorf("%s: failed to create branch %s: %w", repo, branchName, err)
	}

	// Commit changes
	commitMsg := "Update GitHub Actions\n\n"
	for _, u := range updates {
		commitMsg += "- " + u + "\n"
	}

	if err := gitRun(repoDir, "add", "-A"); err != nil {
		return fmt.Errorf("%s: failed to stage changes: %w", repo, err)
	}

	if err := gitRun(repoDir, "commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("%s: failed to commit: %w", repo, err)
	}

	// Push branch
	fmt.Printf("Pushing branch %s...\n", branchName)
	if err := gitRun(repoDir, "push", "-u", "origin", branchName); err != nil {
		return fmt.Errorf("%s: failed to push: %w", repo, err)
	}

	// Create PR
	fmt.Println("Creating PR...")
	prBody := "## Updates\n\n"
	for _, u := range updates {
		prBody += "- " + u + "\n"
	}
	prBody += "\n---\nCreated by mygithelper"

	if err := createPR(repoDir, "Update GitHub Actions", prBody); err != nil {
		return fmt.Errorf("%s: failed to create PR: %w", repo, err)
	}

	// Switch back to default branch
	if err := gitRun(repoDir, "checkout", defaultBranch); err != nil {
		return fmt.Errorf("%s: failed to checkout %s: %w", repo, defaultBranch, err)
	}

	return nil
}

// --- Reset command ---

func runReset() error {
	baseDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	groupsFile := filepath.Join(baseDir, "myrepogroups.txt")
	groups, err := readLines(groupsFile)
	if err != nil {
		return fmt.Errorf("failed to read groups file %s: %w", groupsFile, err)
	}

	for _, group := range groups {
		groupDir := filepath.Join(baseDir, group)
		reposFile := filepath.Join(baseDir, fmt.Sprintf("myrepogroups.%s.txt", group))

		repos, err := readLines(reposFile)
		if err != nil {
			return fmt.Errorf("failed to read repos file %s: %w", reposFile, err)
		}

		for _, repo := range repos {
			parts := strings.Split(repo, "/")
			if len(parts) != 2 {
				continue
			}
			repoName := parts[1]
			repoDir := filepath.Join(groupDir, repoName)

			if !dirExists(repoDir) {
				continue
			}

			fmt.Printf("Resetting %s...\n", repo)
			if err := gitRun(repoDir, "reset", "--hard"); err != nil {
				return fmt.Errorf("%s: git reset --hard failed: %w", repo, err)
			}
		}
	}

	return nil
}

// --- Helpers ---

func getDefaultBranch(repoDir string) (string, error) {
	// Try to get the default branch from remote
	output, err := gitOutput(repoDir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		branch := strings.TrimSpace(output)
		branch = strings.TrimPrefix(branch, "refs/remotes/origin/")
		return branch, nil
	}

	// Fallback: check if main or master exists
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

	// Match "go 1.23" or "go 1.23.4"
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
	_, err := gitOutput(repoDir, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return false
	}
	output, _ := gitOutput(repoDir, "ls-remote", "--heads", "origin", branch)
	return strings.TrimSpace(output) != ""
}

func updateTestYml(repoDir, currentVersion, prevVersion string) (newContent []byte, updated bool, err error) {
	testYmlPath := filepath.Join(repoDir, ".github", "workflows", "test.yml")
	content, err := os.ReadFile(testYmlPath)
	if err != nil {
		return nil, false, fmt.Errorf("no .github/workflows/test.yml found")
	}

	original := string(content)

	// Match go-version: [...] in matrix section
	// This regex matches the go-version line with array syntax
	re := regexp.MustCompile(`(?m)(go-version:\s*)\[([^\]]*)\]`)

	newVersions := fmt.Sprintf("[%s.x, %s.x]", prevVersion, currentVersion)
	result := re.ReplaceAllString(original, "${1}"+newVersions)

	if result == original {
		return nil, false, nil
	}

	newContent = []byte(result)
	if err := os.WriteFile(testYmlPath, newContent, 0644); err != nil {
		return nil, false, fmt.Errorf("failed to write test.yml: %w", err)
	}

	return newContent, true, nil
}

func runGhat(repoDir string) error {
	cmd := exec.Command("ghat", "swot", "-d", ".")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func createPR(repoDir, title, body string) error {
	cmd := exec.Command("gh", "pr", "create", "--title", title, "--body", body)
	cmd.Dir = repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- Git helpers ---

// gitRun runs a git command with stdout/stderr attached.
func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gitOutput runs a git command and returns its output.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	return string(output), err
}

// checkUncommitted returns true if the repo has uncommitted changes.
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

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
