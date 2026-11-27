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
- Textures, models and audio have import settings of their own: `new-meta.sh` refuses them.
  Use the ones already in the project, or say in the pull request what the owner should add.
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
