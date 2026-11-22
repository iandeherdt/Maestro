---
name: godot-dev
description: Work on this Godot 4 project: GDScript, scenes, tests. Use for any change to the game's code, scenes or project settings.
---

# Working on this Godot project

The checks the service runs before anything is pushed are in `conventions/godot/`, and what
they enforce is in `conventions/godot/RULES.md`. Run them yourself while you work:

```sh
sh conventions/godot/check.sh     # quick: version, every script parses, gdformat, gdlint
sh conventions/godot/verify.sh    # everything, with the import and the tests
```

## How to work

- Godot runs without a screen here: `godot --headless`. There is no editor to click in, so
  scenes (`.tscn`) and resources (`.tres`) are edited as the text files they are. Keep their
  format: Godot rewrites a file it does not like, and the check then fails on a changed file.
- After you add or rename a script, scene or asset, run `godot --headless --import`. Godot makes
  a `.uid` file (and an `.import` file for assets) next to it; commit those with it, or
  references to it break.
- Refer to scenes and resources by `uid://` where the project does, and to classes by their
  `class_name`.
- Format with `gdformat` before you finish; `gdlint` says what else it wants.
- Tests use GUT: files named `test_*.gd` under `test/`, `extends GutTest`, functions named
  `test_*`. Add a test for new behaviour, and for a bug you fix.
- `.godot/` is Godot's cache: never commit it, never edit it.
