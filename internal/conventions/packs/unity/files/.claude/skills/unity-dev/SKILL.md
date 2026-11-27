---
name: unity-dev
description: Works on this Unity project without the editor: C# scripts, scenes and prefabs as YAML text, .meta files and GUIDs, Unity Test Framework tests. Use for any change to the game's code, assets or project settings.
---

# Working on this Unity project

The checks the service runs before anything is pushed are in `conventions/unity/`, and what
they enforce is in `conventions/unity/RULES.md`.

You have no Unity licence, so you cannot open the project in Unity or run Unity's own tests
here. The service can: commit and end your turn with `MAESTRO_STATUS: check`, and the next turn
gives you the output of Unity's tests and, when the project takes them, screenshots of each scene
(`../check-screenshots/`), which you open with your file reading tool. Do that before you finish
anything visual, and look at the pictures. What you can run yourself:

```sh
sh conventions/unity/new-meta.sh   # .meta files, with new GUIDs, for what you added
sh conventions/unity/check.sh      # .meta files and GUIDs, and every script compiled with Unity's compiler
```

Run it after every change. It is quicker than a failed check, which costs a whole new attempt.

## Pictures that show your change

On their own, the pictures are each scene a second or two after it starts: the menu, or the first
frame. You know what you built, so the pictures are yours to choose, and the service asks for
them before it publishes. To show what you changed, add a picture set-up: a static method without parameters in a
class named `MaestroShotSetups`, in `Assets/Editor/MaestroShotSetups.cs` (run `new-meta.sh` for
it). The service plays the first scene, calls the method once the game has started, and takes
the picture a second later, or after the seconds the method returns as a `float` (at most 20).
The picture is named after the method. Then name it in your final message, one line each:

```csharp
using UnityEngine;
using UnityEngine.SceneManagement;

static class MaestroShotSetups
{
    // Past the menu, into the first level, with the new enemy in view.
    static float FirstLevelWithEnemy()
    {
        SceneManager.LoadScene("Level1");
        return 2f; // let the level load and settle
    }
}
```

```text
MAESTRO_SHOW: FirstLevelWithEnemy
```

The method runs in play mode, so it can load a scene, press the start button of the menu
(`button.onClick.Invoke()`), move the player or the camera, or start a coroutine on an object of
the game. Keep the set-ups the project already has; they are shown only when named. A scene
can be named too (`MAESTRO_SHOW: Level1`). With `MAESTRO_STATUS: check` you see the pictures
before you finish, and `Logs/maestro-shots.log` says why a set-up failed.

## How to work

- There is no editor to click in. Scenes (`.unity`), prefabs (`.prefab`), materials and
  ScriptableObjects (`.asset`) are YAML text: edit them as text and keep Unity's format. Prefer
  doing work in code and small prefabs over large edits to a scene file.
- Every file and folder under `Assets/` needs a `.meta` file next to it with its own GUID.
  After you add files or folders, run `sh conventions/unity/new-meta.sh` (or give it the new
  paths): it writes the missing `.meta` files with new GUIDs, as the Editor would, and never
  touches an existing one. Do not write or copy `.meta` files by hand, and never change the
  GUID of an existing asset: everything that refers to it breaks. When you move or rename an
  asset, move its `.meta` with it; when you delete one, delete its `.meta`.
- Textures and models you make go in `Assets/Art/Generated/` (see "Making art" below); there
  `new-meta.sh` writes their `.meta` files. Anywhere else it refuses them, and binary files
  anywhere else are refused when you finish. Audio is added by the owner.
- A reference to another asset in YAML is `{fileID: ..., guid: ..., type: ...}`: take the GUID
  from that asset's `.meta` file.
- Do not touch `Packages/manifest.json`, `Packages/packages-lock.json` or `ProjectSettings/`
  version and editor settings: the owner changes packages and the Unity version. Use only the
  packages the project already has.
- Tests use the Unity Test Framework (NUnit): EditMode tests in an assembly under
  `Assets/Tests/` with an `.asmdef` that references `UnityEngine.TestRunner` and
  `UnityEditor.TestRunner`, PlayMode tests in their own assembly. Add a test for new behaviour
  and for a bug you fix, and prefer EditMode tests of plain C# logic: they are fast and sure.
- Keep game logic in plain C# classes where you can, so it can be tested without a scene.
- `Library/`, `Temp/`, `Logs/` and `UserSettings/` are Unity's own: never commit or edit them.

## Making art

You can make textures and models, and you should when the look needs them. Each one comes from a
generator: a script in `ArtGenerators/` (at the top of the repository, not in `Assets/`) that
writes into `Assets/Art/Generated/Textures/` or `Assets/Art/Generated/Models/`. Commit the
generator and what it wrote; binary files are accepted only in `Assets/Art/Generated/`.

`conventions/unity/art.mjs` has what a generator needs, in plain Node: `png` (an RGBA picture
from a function of x and y), `normalFromHeight`, tileable `noise` and `fbm`, and `obj` (a mesh).

```js
// ArtGenerators/concrete.mjs: run with `node ArtGenerators/concrete.mjs`
import { png, fbm, normalFromHeight } from "../conventions/unity/art.mjs";

const dir = "Assets/Art/Generated/Textures";
const height = (x, y) => fbm(x / 64, y / 64, 5, 8, 7);       // tiles every 512 pixels
png(`${dir}/Concrete.png`, 512, 512, (x, y) => {
  const h = height(x, y), stain = fbm(x / 128, y / 128, 3, 4, 3);
  const v = 70 + 90 * h - 40 * Math.max(0, stain - 0.55);
  return [v, v * 0.97, v * 0.93];
});
png(`${dir}/Concrete_Normal.png`, 512, 512, normalFromHeight(height, 6));
```

Then run `sh conventions/unity/new-meta.sh Assets/Art/Generated` and use the texture in a
material (`.mat`, with the GUID from its `.meta`).

- Use a fixed seed, so running a generator again gives the same files; change only the
  generators your task is about, and commit their output.
- Keep sizes sensible: 512 or 1024 pixels square for most textures, 2048 at most.
- Unity imports a new picture as a colour texture. For normal maps and masks, add once an
  editor script `Assets/Editor/GeneratedArtImport.cs` with an `AssetPostprocessor` whose
  `OnPreprocessTexture` sets `TextureImporterType.NormalMap` for names ending in `_Normal`, and
  `sRGBTexture = false` for `_Mask` and `_Height`.
- Models: build meshes in a generator with `obj` (Y up, metres), or, when `blender` is installed
  here (`command -v blender`), with a Blender Python script that exports FBX into
  `Assets/Art/Generated/Models/`. A mesh can also be built in C# at runtime (`Mesh`), which needs
  no file at all. Give models enough shape to read in the picture: bevels, panels, details.
- Look at the result with `MAESTRO_STATUS: check` and a picture set-up that shows it.

## Making it look good

Most of the look comes from light, materials and effects, and a scene of simple shapes looks far
better lit well than a detailed one lit badly. Before you finish anything visual, look at it
with `MAESTRO_STATUS: check` and fix what is too dark, flat or empty.

- **Art first.** Build with what the project has (`Assets/` models, prefabs, materials, asset
  packs) and what you generate; combine shapes into props (pipes, crates, railings, wall panels,
  door frames) rather than leaving bare cubes.
- **Light with a purpose.** One key light, a fill of another colour (warm against cold), and
  practical lights where the scene has lamps. Give lamps an emissive material so the source
  glows. Keep the picture readable: if the pictures are mostly black, raise the light, the
  ambient light or the exposure.
- **Materials.** Textured surfaces with normal maps; set smoothness and metallic deliberately
  and vary them between surfaces.
- **Post-processing.** With URP or HDRP, add a global `Volume` with bloom, tonemapping, colour
  adjustments and a vignette. Use fog (`RenderSettings.fog`) for depth.
- **Effects.** Particle systems for muzzle flashes, sparks, smoke and dust, with a light that
  flashes with them. Effects started from code show in the picture only if the picture set-up
  triggers them.
