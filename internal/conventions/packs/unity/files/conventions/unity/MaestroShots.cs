// Takes pictures of the game for the pull request. conventions/unity/previews.sh
// copies this file into Assets/ for one run of the Editor and the service
// removes it again afterwards; it is never part of the project.
//
// Each scene is opened and played for a moment, so that what the game builds
// when it starts (menus, the level, the player) is there, and then every
// camera is drawn into one picture. Canvases that draw over the screen are
// drawn by the last camera for that moment, because a camera on its own
// leaves them out.
//
// A picture of the first frame rarely shows what a change is about, so the
// project can set up its own pictures: every static method without
// parameters of a class named MaestroShotSetups (in any namespace, usually
// Assets/Editor/MaestroShotSetups.cs) is one picture. The first scene is
// played, the method is called once the game has started, and the picture
// is taken a second later, or after the seconds the method returns (a float,
// at most 20). The method can load a level, place the player, open a menu or
// start a coroutine on an object of the game. The picture is named after the
// method. SHOT_SHOW (names separated by '|') picks the methods and scenes to
// show; without it every set-up is shown before the plain scenes.
#if UNITY_EDITOR
using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Reflection;
using UnityEditor;
using UnityEditor.SceneManagement;
using UnityEngine;
using UnityEngine.Rendering;

// Play mode may reload the scripts, which empties every static field, so
// the state of the run is kept in SessionState and the update callback is
// registered again after a reload.
[InitializeOnLoad]
public static class MaestroShots
{
    const int MaxPictures = 6;
    // Below what the service takes out of the sandbox (700 KB).
    const int MaxBytes = 680 * 1024;
    const double SettleSeconds = 2.0;
    const double SceneTimeoutSeconds = 90.0;
    const double SetupWaitSeconds = 1.0;
    const double MaxSetupWaitSeconds = 20.0;
    const string SetupClass = "MaestroShotSetups";
    const string K = "MaestroShots.";

    static bool Running { get { return SessionState.GetBool(K + "running", false); } set { SessionState.SetBool(K + "running", value); } }
    static string Queue { get { return SessionState.GetString(K + "queue", ""); } set { SessionState.SetString(K + "queue", value); } }
    static string Dir { get { return SessionState.GetString(K + "dir", ""); } set { SessionState.SetString(K + "dir", value); } }
    static int Width { get { return SessionState.GetInt(K + "width", 1280); } set { SessionState.SetInt(K + "width", value); } }
    static int Height { get { return SessionState.GetInt(K + "height", 720); } set { SessionState.SetInt(K + "height", value); } }
    static int Taken { get { return SessionState.GetInt(K + "taken", 0); } set { SessionState.SetInt(K + "taken", value); } }
    static int Number { get { return SessionState.GetInt(K + "number", 0); } set { SessionState.SetInt(K + "number", value); } }
    static float Started { get { return SessionState.GetFloat(K + "started", 0); } set { SessionState.SetFloat(K + "started", value); } }
    static bool Playing { get { return SessionState.GetBool(K + "playing", false); } set { SessionState.SetBool(K + "playing", value); } }
    static bool Shot { get { return SessionState.GetBool(K + "shot", false); } set { SessionState.SetBool(K + "shot", value); } }
    // The set-up of the picture being taken: "type\tmethod", empty for a
    // plain scene.
    static string Setup { get { return SessionState.GetString(K + "setup", ""); } set { SessionState.SetString(K + "setup", value); } }
    static bool Called { get { return SessionState.GetBool(K + "called", false); } set { SessionState.SetBool(K + "called", value); } }
    static float CalledAt { get { return SessionState.GetFloat(K + "calledAt", 0); } set { SessionState.SetFloat(K + "calledAt", value); } }
    static float Wait { get { return SessionState.GetFloat(K + "wait", 0); } set { SessionState.SetFloat(K + "wait", value); } }

    static MaestroShots()
    {
        if (Running) EditorApplication.update += Tick;
    }

    static float Now { get { return (float)EditorApplication.timeSinceStartup; } }

    // Called with -executeMethod MaestroShots.Run.
    public static void Run()
    {
        try
        {
            var dir = Environment.GetEnvironmentVariable("SCREENSHOT_DIR");
            if (string.IsNullOrEmpty(dir)) throw new Exception("SCREENSHOT_DIR is not set");
            Directory.CreateDirectory(dir);
            Dir = dir;
            Width = Size("SHOT_WIDTH", 1280);
            Height = Size("SHOT_HEIGHT", 720);
            Taken = 0;
            Number = 0;
            Queue = string.Join("\n", Jobs().ToArray());
            if (Queue == "") throw new Exception("the project has no scene");
            Running = true;
            EditorApplication.update += Tick;
            Next();
        }
        catch (Exception e)
        {
            Debug.LogError("MaestroShots: " + e.Message);
            EditorApplication.Exit(1);
        }
    }

    static int Size(string name, int fallback)
    {
        int v;
        return int.TryParse(Environment.GetEnvironmentVariable(name), out v) && v >= 64 && v <= 4000 ? v : fallback;
    }

    // The scenes named in SHOT_SCENES (separated by ':'), else the scenes
    // of the build, else every scene under Assets/.
    static IEnumerable<string> Scenes()
    {
        var named = (Environment.GetEnvironmentVariable("SHOT_SCENES") ?? "")
            .Split(new[] { ':' }, StringSplitOptions.RemoveEmptyEntries).Select(s => s.Trim()).Where(File.Exists).ToList();
        if (named.Count > 0) return named.Take(MaxPictures);
        var build = EditorBuildSettings.scenes.Where(s => s.enabled && File.Exists(s.path)).Select(s => s.path).ToList();
        if (build.Count > 0) return build.Take(MaxPictures);
        return AssetDatabase.FindAssets("t:Scene", new[] { "Assets" }).Select(AssetDatabase.GUIDToAssetPath)
            .OrderBy(p => p, StringComparer.Ordinal).Take(MaxPictures);
    }

    // What to take, one line each: "scene\tpath" for a plain scene and
    // "setup\tpath\ttype\tmethod" for a set-up, at most MaxPictures.
    static IEnumerable<string> Jobs()
    {
        var scenes = Scenes().ToList();
        if (scenes.Count == 0) return new string[0];
        var show = (Environment.GetEnvironmentVariable("SHOT_SHOW") ?? "")
            .Split(new[] { '|' }, StringSplitOptions.RemoveEmptyEntries)
            .Select(s => Key(Path.GetFileNameWithoutExtension(s.Trim()))).Where(k => k != "").ToList();
        var setups = Setups().Select(m => new { key = Key(m.Name), line = "setup\t" + scenes[0] + "\t" + m.DeclaringType.AssemblyQualifiedName + "\t" + m.Name }).ToList();
        var plain = scenes.Select(p => new { key = Key(Path.GetFileNameWithoutExtension(p)), line = "scene\t" + p }).ToList();
        var jobs = new List<string>();
        if (show.Count > 0)
        {
            // What the change is about, in the order given.
            foreach (var k in show)
            {
                jobs.AddRange(setups.Where(j => j.key == k).Select(j => j.line));
                jobs.AddRange(plain.Where(j => j.key == k).Select(j => j.line));
            }
            jobs = jobs.Distinct().ToList();
        }
        if (jobs.Count == 0)
        {
            if (show.Count > 0) Debug.LogWarning("MaestroShots: nothing is named " + string.Join(", ", show.ToArray()) + "; showing every set-up and scene");
            jobs.AddRange(setups.Select(j => j.line));
            jobs.AddRange(plain.Select(j => j.line));
        }
        return jobs.Take(MaxPictures);
    }

    // A name as it is compared: letters and digits only, in lower case.
    static string Key(string name)
    {
        return new string((name ?? "").Where(char.IsLetterOrDigit).Select(char.ToLowerInvariant).ToArray());
    }

    static IEnumerable<MethodInfo> Setups()
    {
        var found = new List<MethodInfo>();
        foreach (var asm in AppDomain.CurrentDomain.GetAssemblies())
        {
            Type[] types;
            try { types = asm.GetTypes(); }
            catch (ReflectionTypeLoadException e) { types = e.Types.Where(t => t != null).ToArray(); }
            catch (Exception) { continue; }
            foreach (var t in types)
            {
                if (t == null || t.Name != SetupClass) continue;
                found.AddRange(t.GetMethods(BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Static | BindingFlags.DeclaredOnly)
                    .Where(m => !m.IsSpecialName && !m.ContainsGenericParameters && m.GetParameters().Length == 0)
                    .OrderBy(m => m.MetadataToken));
            }
        }
        return found;
    }

    static void Next()
    {
        var queue = Queue.Split(new[] { '\n' }, StringSplitOptions.RemoveEmptyEntries).ToList();
        if (queue.Count == 0)
        {
            EditorApplication.update -= Tick;
            Running = false;
            Debug.Log("MaestroShots: " + Taken + " picture(s) taken");
            EditorApplication.Exit(Taken > 0 ? 0 : 1);
            return;
        }
        Queue = string.Join("\n", queue.Skip(1).ToArray());
        Number = Number + 1;
        var job = queue[0].Split('\t');
        Setup = job.Length >= 4 && job[0] == "setup" ? job[2] + "\t" + job[3] : "";
        try
        {
            EditorSceneManager.OpenScene(job.Length > 1 ? job[1] : job[0], OpenSceneMode.Single);
        }
        catch (Exception e)
        {
            Debug.LogWarning("MaestroShots: could not open " + queue[0] + ": " + e.Message);
            Next();
            return;
        }
        Started = Now;
        Playing = false;
        Shot = false;
        Called = false;
        EditorApplication.isPlaying = true;
    }

    static void Tick()
    {
        if (!Running) return;
        var now = Now;
        if (!Playing)
        {
            if (EditorApplication.isPlaying)
            {
                Playing = true;
                Started = now;
            }
            else if (now - Started > SceneTimeoutSeconds)
            {
                // Play mode did not start: a picture of the scene as saved,
                // which a set-up cannot change.
                if (Setup == "") Shoot();
                else Debug.LogWarning("MaestroShots: play mode did not start for " + Setup.Replace('\t', '.'));
                Next();
            }
            return;
        }
        if (!EditorApplication.isPlaying)
        {
            // Play mode ended: after the picture, or the game left it by
            // itself.
            Next();
            return;
        }
        if (Shot || now - Started < SettleSeconds) return;
        if (Setup != "")
        {
            if (!Called)
            {
                Called = true;
                CalledAt = now;
                Wait = (float)SetupWaitSeconds;
                if (!CallSetup())
                {
                    Shot = true;
                    EditorApplication.isPlaying = false;
                }
                return;
            }
            if (now - CalledAt < Wait) return;
        }
        Shoot();
        Shot = true;
        EditorApplication.isPlaying = false;
    }

    // Calls the set-up of this picture; false when there is none to call or
    // it failed, and then there is no picture.
    static bool CallSetup()
    {
        var parts = Setup.Split('\t');
        var label = parts.Length > 1 ? parts[1] : Setup;
        try
        {
            var type = Type.GetType(parts[0], false);
            var method = type == null ? null : type.GetMethod(parts[1], BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Static | BindingFlags.DeclaredOnly, null, Type.EmptyTypes, null);
            if (method == null)
            {
                Debug.LogWarning("MaestroShots: set-up " + label + " not found");
                return false;
            }
            var back = method.Invoke(null, null);
            double wait;
            if (back is float) wait = (float)back;
            else if (back is double) wait = (double)back;
            else if (back is int) wait = (int)back;
            else wait = SetupWaitSeconds;
            Wait = (float)Math.Max(0, Math.Min(MaxSetupWaitSeconds, wait));
            return true;
        }
        catch (Exception e)
        {
            var inner = e is TargetInvocationException && e.InnerException != null ? e.InnerException : e;
            Debug.LogWarning("MaestroShots: set-up " + label + " failed: " + inner.GetType().Name + ": " + inner.Message);
            return false;
        }
    }

    static void Shoot()
    {
        var scene = UnityEngine.SceneManagement.SceneManager.GetActiveScene().name;
        if (Setup != "")
        {
            var parts = Setup.Split('\t');
            scene = parts.Length > 1 ? parts[1] : scene;
        }
        try
        {
            int width = Width, height = Height;
            var png = Picture(width, height);
            for (var scale = 0.75f; png != null && png.Length > MaxBytes && scale > 0.2f; scale *= 0.75f)
                png = Picture((int)(width * scale), (int)(height * scale));
            if (png == null || png.Length > MaxBytes)
            {
                Debug.LogWarning("MaestroShots: no picture of " + scene);
                return;
            }
            var name = Number + "-" + new string(scene.Select(c => char.IsLetterOrDigit(c) ? c : '-').ToArray());
            File.WriteAllBytes(Path.Combine(Dir, name + ".png"), png);
            Taken = Taken + 1;
        }
        catch (Exception e)
        {
            Debug.LogWarning("MaestroShots: no picture of " + scene + ": " + e.Message);
        }
    }

    static byte[] Picture(int w, int h)
    {
        var cameras = UnityEngine.Object.FindObjectsByType<Camera>(FindObjectsSortMode.None)
            .Where(c => c.enabled && c.gameObject.activeInHierarchy && c.targetTexture == null)
            .OrderBy(c => c.depth).ToList();
        if (cameras.Count == 0) return null;
        var top = cameras[cameras.Count - 1];

        var overlays = UnityEngine.Object.FindObjectsByType<Canvas>(FindObjectsSortMode.None)
            .Where(c => c.isRootCanvas && c.renderMode == RenderMode.ScreenSpaceOverlay)
            .Select(c => new Overlay { canvas = c, distance = c.planeDistance }).ToList();
        var rt = RenderTexture.GetTemporary(w, h, 24, RenderTextureFormat.ARGB32);
        var before = RenderTexture.active;
        var targets = cameras.Select(c => c.targetTexture).ToList();
        try
        {
            // Each camera draws into the picture, so canvases are laid out
            // for its size and not for the (absent) game view.
            foreach (var cam in cameras) cam.targetTexture = rt;
            foreach (var o in overlays)
            {
                o.canvas.renderMode = RenderMode.ScreenSpaceCamera;
                o.canvas.worldCamera = top;
                o.canvas.planeDistance = top.nearClipPlane + 0.1f;
            }
            Canvas.ForceUpdateCanvases();
            RenderTexture.active = rt;
            GL.Clear(true, true, Color.black);
            foreach (var cam in cameras) Draw(cam, rt);
            var tex = new Texture2D(w, h, TextureFormat.RGB24, false);
            RenderTexture.active = rt;
            tex.ReadPixels(new Rect(0, 0, w, h), 0, 0);
            tex.Apply();
            var png = tex.EncodeToPNG();
            UnityEngine.Object.DestroyImmediate(tex);
            return png;
        }
        finally
        {
            RenderTexture.active = before;
            for (var i = 0; i < cameras.Count; i++) cameras[i].targetTexture = targets[i];
            RenderTexture.ReleaseTemporary(rt);
            foreach (var o in overlays)
            {
                o.canvas.renderMode = RenderMode.ScreenSpaceOverlay;
                o.canvas.worldCamera = null;
                o.canvas.planeDistance = o.distance;
            }
        }
    }

    class Overlay
    {
        public Canvas canvas;
        public float distance;
    }

    static void Draw(Camera cam, RenderTexture rt)
    {
        try
        {
            if (GraphicsSettings.currentRenderPipeline != null)
            {
                // A render pipeline (URP, HDRP) draws a camera on request.
                var req = new RenderPipeline.StandardRequest { destination = rt };
                if (RenderPipeline.SupportsRenderRequest(cam, req))
                {
                    RenderPipeline.SubmitRenderRequest(cam, req);
                    return;
                }
            }
            cam.Render();
        }
        catch (Exception e)
        {
            // An overlay camera of a stack is drawn by its base camera.
            Debug.LogWarning("MaestroShots: camera " + cam.name + " not drawn: " + e.Message);
        }
    }
}
#endif
