---
name: blender-art
description: Make or change 3D art in this project. The models come from Python generator scripts that Blender runs without a screen. Use when a task asks for a new model, prop, character or level piece, or a change to one.
---

# 3D art made by scripts

The models of this project are not drawn by hand. Each one comes from a Python script that
Blender runs without a screen. The scripts are the source; what they make is output.

## The contract

- A generator is one `.py` file in the folder named by `ART_SOURCE` in
  `conventions/blender/settings.sh`. A file whose name starts with `_` is shared code that
  generators import, not a generator.
- Blender runs it as
  `blender -b --factory-startup --python-exit-code 1 -P GENERATOR.py -- --out DIR [--preview DIR]`.
- It builds its piece from an empty scene, writes its files (`.fbx`, `.glb`, `.gltf` or `.obj`,
  and textures if it needs them) into `--out`, and nothing anywhere else.
- When `--preview` is given it renders one or more PNG pictures into that folder.
- Any exception fails it. Do not catch errors to keep going.

## A generator

```python
import bpy
import maestro_art as art

out, preview = art.args()
art.empty_scene()
bpy.ops.mesh.primitive_cube_add(size=1, location=(0, 0, 0.5))
crate = bpy.context.active_object
crate.name = "Crate"
crate.data.materials.append(art.material("Wood", (0.55, 0.35, 0.2, 1)))
art.export_fbx(f"{out}/Crate.fbx")
if preview:
    art.render_preview(f"{preview}/crate.png")
```

`maestro_art` (in `conventions/blender/`) has `args`, `empty_scene`, `material` (a colour that shows in renders and exports), `export_fbx` (axes and scale
for Unity by default), `export_gltf` and `render_preview`.

## Commands

```sh
sh conventions/blender/build.sh            # every generator, into ART_OUTPUT
sh conventions/blender/build.sh crate      # only this one
sh conventions/blender/previews.sh /tmp/p  # pictures of the generators you changed
sh conventions/blender/verify.sh           # the checks, as the service runs them
```

Look at the pictures before you finish: they are how you see what you made. Commit the generator
and the files `build.sh` wrote for it (only for the generators you changed: every export carries
the time it was made, so rebuilding others changes them for nothing).

## How to work

1. Read two or three existing generators first, and the shared code they import, so the new one
   matches their scale, style, colours and naming.
2. Write or change the generator. Keep it a script that builds the piece from nothing: no
   files loaded from disk except shared code and textures in the repository.
3. Build only that piece: `sh conventions/blender/build.sh NAME`. A Python error is shown with
   its line.
4. Render it: `sh conventions/blender/previews.sh /tmp/previews`, then look at the PNG files
   (read them as images). Check the shape, the proportions next to the existing pieces, the
   colours and that nothing is inside out or floating. Fix and render again until it looks
   right; say in the pull request what you checked.
5. Run `sh conventions/blender/verify.sh` before you finish.

Scale and axes: one Blender unit is one metre. `export_fbx` writes the axes Unity expects, so
model with Z up in Blender as usual and do not rotate the piece to compensate.
