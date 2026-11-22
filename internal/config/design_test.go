package config

import (
	"slices"
	"strings"
	"testing"
)

const onBoard = "  kind: github\n  provider:\n    state_source: project\n"

func TestDesignStageIsOnWithPlanningOnABoard(t *testing.T) {
	c := mustBuild(t, with("  kind: github\n", onBoard, ""))
	if c.Tracker.DesignState != "Design" || !slices.Contains(c.Tracker.ActiveStates, "Design") {
		t.Fatalf("design state %q, active %v", c.Tracker.DesignState, c.Tracker.ActiveStates)
	}
	if c.Design.Kind != "auto" || c.Design.Folder != "design" {
		t.Errorf("design %+v", c.Design)
	}
	// With labels there is no planning, so no design either.
	if c := mustBuild(t, minimal); c.Tracker.DesignState != "" || slices.Contains(c.Tracker.ActiveStates, "Design") {
		t.Errorf("labels: design state %q", c.Tracker.DesignState)
	}
	// It can be switched off.
	c = mustBuild(t, with("  kind: github\n", onBoard+"  design_state: \"\"\n", ""))
	if c.Tracker.DesignState != "" || slices.Contains(c.Tracker.ActiveStates, "Design") {
		t.Errorf("switched off: %q %v", c.Tracker.DesignState, c.Tracker.ActiveStates)
	}
	c = mustBuild(t, with("  kind: github\n", onBoard, "design:\n  kind: game\n  folder: docs/design/\n"))
	if c.Design.Kind != "game" || c.Design.Folder != "docs/design" {
		t.Errorf("design %+v", c.Design)
	}
}

func TestDesignSettingsThatAreRefused(t *testing.T) {
	for name, c := range map[string]struct{ front, want string }{
		"same as planned":  {with("  kind: github\n", onBoard+"  design_state: Planned\n", ""), "tracker.design_state"},
		"same as active":   {with("  kind: github\n", onBoard+"  design_state: Todo\n", ""), "tracker.design_state"},
		"without planning": {with("  kind: github\n", "  kind: github\n  design_state: Design\n", ""), "needs tracker.planning_state"},
		"kind":             {with("  kind: github\n", onBoard, "design:\n  kind: film\n"), "design.kind"},
		"folder up":        {with("  kind: github\n", onBoard, "design:\n  folder: ../x\n"), "design.folder"},
		"folder absolute":  {with("  kind: github\n", onBoard, "design:\n  folder: /x\n"), "design.folder"},
		"unknown key":      {with("  kind: github\n", onBoard, "design:\n  colour: red\n"), "colour"},
	} {
		_, err := build(t, c.front, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
