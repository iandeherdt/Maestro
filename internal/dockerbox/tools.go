package dockerbox

// Tool is a program the sandbox image can add by name (sandbox.tools). A
// tool needs no secret and no network once it is in the image.
type Tool struct {
	// Lines are Dockerfile lines. They run as root, before the image
	// switches to the agent's user.
	Lines []string
	// Note says in words what the tool adds, for `maestro image`.
	Note string
}

// BlenderVersion is the Blender release the image gets.
const BlenderVersion = "5.0.1"

// Tools are the tools by name. Every name is also in config.SandboxTools.
var Tools = map[string]Tool{
	// Blender runs headless (blender -b), for projects whose 3D models
	// are made by Python scripts. The release comes from blender.org and
	// is checked against the checksum file published next to it. BLENDER
	// names the program for scripts that look for it there.
	"blender": {
		Note: "Blender " + BlenderVersion + " (about 1 GB), to run without a screen",
		Lines: []string{
			"ARG TARGETARCH",
			`RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends xz-utils libgl1 libopengl0 libegl1 libsm6 libice6 libxi6 \
      libxkbcommon0 libxrender1 libxt6 libxfixes3 libxext6 libx11-6 libxxf86vm1; \
    rm -rf /var/lib/apt/lists/*; \
    case "$TARGETARCH" in amd64) arch=x64 ;; arm64) arch=arm64 ;; *) echo "Blender has no build for $TARGETARCH" >&2; exit 1 ;; esac; \
    base="https://download.blender.org/release/Blender` + "5.0" + `"; \
    file="blender-` + BlenderVersion + `-linux-$arch.tar.xz"; \
    cd /tmp; \
    curl -fsSL --proto '=https' --tlsv1.2 -o "$file" "$base/$file"; \
    curl -fsSL --proto '=https' --tlsv1.2 -o blender.sha256 "$base/blender-` + BlenderVersion + `.sha256"; \
    grep " $file\$" blender.sha256 | sha256sum -c -; \
    mkdir -p /opt/blender; \
    tar -xJf "$file" -C /opt/blender --strip-components=1; \
    rm -f "$file" blender.sha256; \
    ln -s /opt/blender/blender /usr/local/bin/blender`,
			"ENV BLENDER=/opt/blender/blender",
		},
	},
}
