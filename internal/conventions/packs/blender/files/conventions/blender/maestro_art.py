"""Helpers for the generators of the blender conventions pack.

A generator is a Python script that Blender runs without a screen:

    blender -b --python-exit-code 1 -P art/blender/crate.py -- --out DIR [--preview DIR]

It builds its piece from nothing, writes the files into --out, and when
--preview is given renders one or more PNG pictures into that folder. An
exception fails it. See .claude/skills/blender-art/SKILL.md.
"""

import argparse
import math
import os
import sys

import bpy


def args():
    """The folders the generator writes to: (out, preview or None)."""
    argv = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else []
    p = argparse.ArgumentParser(prog="generator")
    p.add_argument("--out", required=True)
    p.add_argument("--preview")
    a = p.parse_args(argv)
    os.makedirs(a.out, exist_ok=True)
    if a.preview:
        os.makedirs(a.preview, exist_ok=True)
    return a.out, a.preview


def empty_scene():
    """Starts from an empty scene, so nothing of a previous run is left."""
    bpy.ops.wm.read_factory_settings(use_empty=True)
    return bpy.context.scene


def material(name, rgba, roughness=0.6, metallic=0.0):
    """A material whose colour shows in renders and travels in FBX and glTF
    exports (the base colour of its Principled BSDF), and in the viewport."""
    m = bpy.data.materials.new(name)
    m.diffuse_color = rgba
    m.use_nodes = True
    bsdf = m.node_tree.nodes.get("Principled BSDF")
    if bsdf:
        bsdf.inputs["Base Color"].default_value = rgba
        bsdf.inputs["Roughness"].default_value = roughness
        bsdf.inputs["Metallic"].default_value = metallic
    return m


def export_fbx(path, objects=None, unity=True):
    """Writes FBX. With unity=True the axes and scale are the ones Unity
    expects, so the model imports with no fixes."""
    _select(objects)
    kwargs = dict(filepath=path, use_selection=objects is not None, apply_unit_scale=True)
    if unity:
        kwargs.update(axis_forward="-Z", axis_up="Y", apply_scale_options="FBX_SCALE_UNITS", bake_space_transform=True)
    bpy.ops.export_scene.fbx(**kwargs)


def export_gltf(path, objects=None):
    """Writes glTF binary (.glb)."""
    _select(objects)
    bpy.ops.export_scene.gltf(filepath=path, use_selection=objects is not None)


def render_preview(path, size=(640, 480), samples=16):
    """Renders the scene from a camera that frames every mesh, lit by a
    sun, into a PNG file."""
    scene = bpy.context.scene
    meshes = [o for o in scene.objects if o.type == "MESH"]
    lo = [min(v[i] for o in meshes for v in _corners(o)) for i in range(3)] if meshes else [-1, -1, -1]
    hi = [max(v[i] for o in meshes for v in _corners(o)) for i in range(3)] if meshes else [1, 1, 1]
    centre = [(a + b) / 2 for a, b in zip(lo, hi)]
    radius = max(max(b - a for a, b in zip(lo, hi)), 0.1)
    cam = bpy.data.objects.new("maestro_preview_camera", bpy.data.cameras.new("maestro_preview_camera"))
    scene.collection.objects.link(cam)
    d = radius * 3.2
    cam.location = (centre[0] + d * 0.7, centre[1] - d * 0.7, centre[2] + d * 0.5)
    direction = [c - l for c, l in zip(centre, cam.location)]
    cam.rotation_euler = (math.atan2(math.hypot(direction[0], direction[1]), -direction[2]), 0,
                          math.atan2(direction[1], direction[0]) - math.pi / 2)
    sun = bpy.data.objects.new("maestro_preview_sun", bpy.data.lights.new("maestro_preview_sun", "SUN"))
    sun.rotation_euler = (0.8, 0.2, 0.6)
    scene.collection.objects.link(sun)
    scene.camera = cam
    scene.render.engine = "CYCLES"
    scene.cycles.device = "CPU"
    scene.cycles.samples = samples
    scene.render.resolution_x, scene.render.resolution_y = size
    scene.render.filepath = path
    bpy.ops.render.render(write_still=True)
    for o in (cam, sun):
        bpy.data.objects.remove(o)


def _corners(o):
    return [o.matrix_world @ __import__("mathutils").Vector(c) for c in o.bound_box]


def _select(objects):
    for o in bpy.context.scene.objects:
        o.select_set(objects is None or o in objects)
