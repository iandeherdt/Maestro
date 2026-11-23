package sandbox

import (
	"regexp"
	"strings"
)

// The names a request may carry. Every backend checks them the same way,
// and the systemd launcher, which runs as root, relies on them.

var (
	keyRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	workerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
	shaRE    = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	envRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	credRE   = regexp.MustCompile(`^(ANTHROPIC_.*|CLAUDE_.*TOKEN.*|.*_API_KEY|GITHUB_.*|GH_.*|LINEAR_.*)$`)
)

// ValidKey reports whether s can name a workspace: one path element.
func ValidKey(s string) bool { return keyRE.MatchString(s) && !strings.Contains(s, "..") }

// ValidWorker reports whether s can name a worker in unit and container
// names.
func ValidWorker(s string) bool { return workerRE.MatchString(s) }

// ValidBranch reports whether s is a branch name the scripts accept.
func ValidBranch(s string) bool {
	return branchRE.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".lock") && !strings.HasSuffix(s, ".")
}

// ValidCommit reports whether s is a full lowercase object name.
func ValidCommit(s string) bool { return shaRE.MatchString(s) }

// Credential reports whether an environment variable name looks like it
// holds a credential. Such a variable never enters the sandbox.
func Credential(name string) bool { return credRE.MatchString(strings.ToUpper(name)) }

// AllowedEnv reports whether a request may set the variable k to v.
func AllowedEnv(k, v string) bool {
	return envRE.MatchString(k) && !Credential(k) && !strings.ContainsAny(v, "\x00\n")
}
