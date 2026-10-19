"use strict";

const api = globalThis.browser ?? globalThis.chrome;
const HOST = "io.github.mustafaberatdemirci.siphon";
const DEFAULTS = {
  capture: true,
  types: "zip rar 7z tar gz tgz bz2 xz zst iso img dmg exe msi apk deb rpm mp4 mkv avi mov webm m4v mp3 flac m4a wav ogg opus pdf epub",
  minSizeMB: 1,
  exclude: "",
};

const $ = (id) => document.getElementById(id);

async function load() {
  const s = { ...DEFAULTS, ...(await api.storage.local.get(DEFAULTS)) };
  $("capture").checked = s.capture;
  $("types").value = s.types;
  $("minSizeMB").value = s.minSizeMB;
  $("exclude").value = s.exclude.split(/[\s,]+/).filter(Boolean).join("\n");
}

function save() {
  api.storage.local.set({
    capture: $("capture").checked,
    types: $("types").value.trim(),
    minSizeMB: Math.max(0, Number($("minSizeMB").value) || 0),
    exclude: $("exclude").value.trim(),
  });
}

for (const id of ["capture", "types", "minSizeMB", "exclude"]) $(id).addEventListener("change", save);

$("ping").addEventListener("click", async () => {
  const status = $("status");
  status.textContent = "…";
  try {
    const r = await api.runtime.sendNativeMessage(HOST, { type: "ping" });
    status.textContent = r && r.ok ? "Connected to Siphon " + (r.version || "") : "Siphon didn't answer";
  } catch (e) {
    status.textContent = "Siphon can't be reached: install the browser integration in Siphon > Settings. (" + ((e && e.message) || e) + ")";
  }
});

load();
