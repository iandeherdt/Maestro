package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// The self-test (SPEC 11.6) runs `maestro probe` in the sandbox, the way a
// step runs, once per network profile. Both backends share what follows.

// ProbeCheck is the answer to one check.
type ProbeCheck struct {
	// Profile is the network profile the probe ran with: none, agent or
	// hooks.
	Profile string `json:"profile"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	// Advisory checks are reported and never stop the service.
	Advisory bool   `json:"advisory,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// SelfTestReport is what a self-test found.
type SelfTestReport struct {
	Checks []ProbeCheck
	// OldImage is set when the sandbox's maestro has no probe: it was
	// built by an older Maestro. No check ran then.
	OldImage bool
}

// Failed lists the required checks that did not pass.
func (r SelfTestReport) Failed() []ProbeCheck {
	var out []ProbeCheck
	for _, c := range r.Checks {
		if !c.OK && !c.Advisory {
			out = append(out, c)
		}
	}
	return out
}

// Warnings lists the advisory checks that did not pass.
func (r SelfTestReport) Warnings() []ProbeCheck {
	var out []ProbeCheck
	for _, c := range r.Checks {
		if !c.OK && c.Advisory {
			out = append(out, c)
		}
	}
	return out
}

// CanaryName is set in the environment of whatever starts the sandbox
// during the self-test. The probe must not see it.
const CanaryName = "MAESTRO_SELFTEST_CANARY"

// ProbeDenyHost is a host no allowlist has a reason to name.
const ProbeDenyHost = "example.org"

// ParseProbe reads the probe's JSON lines. The probe runs in the sandbox:
// it says what it found, not which profile it ran with, so the caller's
// profile is set on each check.
func ParseProbe(out []byte, profile string) ([]ProbeCheck, error) {
	var checks []ProbeCheck
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var c ProbeCheck
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}
		c.Profile = profile
		checks = append(checks, c)
	}
	return checks, sc.Err()
}
