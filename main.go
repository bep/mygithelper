package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("Usage: mygithelper <command>\nCommands:\n  get    Clone or pull all repos in all groups")
	}

	switch os.Args[1] {
	case "get":
		if err := runGet(); err != nil {
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
