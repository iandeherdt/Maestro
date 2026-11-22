---
name: klad-art
description: Paint or change 2D art in this project (textures, sprites, icons, props) with klad. Use when a task asks for a new picture or a change to one.
---

# 2D art painted with klad

The 2D art of this project is painted with klad, stroke by stroke, through its command line. A
drawing is a `.klad` folder of plain files; the pictures the game uses are exported from it.

## First

Run these and read what they print before you paint. They are klad's own skills: the commands,
and the craft of drawing.

```sh
klad skill            # the tool: looking at a drawing, changing it, checking the result
klad skill drawing    # the craft: planning, light on form, values, colour, a critique checklist
klad skill pixel      # for pixel art
```

## This project

- Drawings live in the folder named by `KLAD_SOURCE` in `conventions/klad/settings.sh`, in
  subfolders too: `art/klad/props/crate.klad`.
- Each drawing is exported to `KLAD_OUTPUT` under the same name: `art/textures/props/crate.png`.
  A drawing that needs other export options has them on one line in a file next to it,
  `props/crate.export`, for example `--sheet all --scale 0.5` for an animation.
- Commit the drawing and its exported picture together. The check exports every drawing again
  and fails when a committed picture is not exactly what its drawing gives: klad renders the
  same pixels on every machine.

## How to work

1. Look at two or three existing drawings first (`klad render`, then read the PNG as an image),
   so the new one matches their size, palette and style.
2. Paint with the `klad` commands, as `klad skill` explains.
3. Look at the result: `klad render art/klad/props/crate.klad -o /tmp/crate.png`, and read the
   picture. Fix and look again until it is right; say in the pull request what you checked.
4. Export it: `sh conventions/klad/build.sh props/crate`.
5. Run `sh conventions/klad/verify.sh` before you finish.
