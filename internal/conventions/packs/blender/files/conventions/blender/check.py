"""Run by verify.sh inside Blender: every file a generator made loads back
into Blender, holds at least one mesh, and stays within the triangle limit.

    blender -b --python-exit-code 1 -P conventions/blender/check.py -- DIR
"""

import os
import sys

import bpy

LOADERS = {
    ".fbx": lambda p: bpy.ops.import_scene.fbx(filepath=p),
    ".glb": lambda p: bpy.ops.import_scene.gltf(filepath=p),
    ".gltf": lambda p: bpy.ops.import_scene.gltf(filepath=p),
    ".obj": lambda p: bpy.ops.wm.obj_import(filepath=p),
}

folder = sys.argv[sys.argv.index("--") + 1]
limit = int(os.environ.get("ART_MAX_TRIANGLES", "0") or 0)
failed = []
for root, _, files in os.walk(folder):
    for name in sorted(files):
        path = os.path.join(root, name)
        rel = os.path.relpath(path, folder)
        ext = os.path.splitext(name)[1].lower()
        if ext not in LOADERS:
            continue
        bpy.ops.wm.read_factory_settings(use_empty=True)
        try:
            LOADERS[ext](path)
        except Exception as e:  # the message is the finding
            failed.append(f"{rel}: does not load in Blender: {e}")
            continue
        meshes = [o for o in bpy.context.scene.objects if o.type == "MESH"]
        if not meshes:
            failed.append(f"{rel}: has no mesh")
            continue
        tris = 0
        for o in meshes:
            o.data.calc_loop_triangles()
            tris += len(o.data.loop_triangles)
        print(f"CHECK {rel}: {len(meshes)} meshes, {tris} triangles")
        if limit and tris > limit:
            failed.append(f"{rel}: {tris} triangles, the limit is {limit}")
for f in failed:
    print("FAILED " + f, file=sys.stderr)
if failed:
    sys.exit(1)
