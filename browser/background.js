// Siphon's browser extension: it hands links, a page's file links and
// downloads to Siphon through its native messaging host (Siphon's command
// line program, registered by Siphon > Settings > Browser integration).
// Siphon opens its Add dialog with them; nothing is queued before the user
// presses Add there. Which of a page's links are files of a supported site is
// Siphon's call (its sites.toml), not this script's.
"use strict";

const api = globalThis.browser ?? globalThis.chrome;
const HOST = "io.github.mustafaberatdemirci.siphon";
const TITLE = "Send this page's file links to Siphon";

// The download capture's rules; the options page changes them.
const DEFAULTS = {
  capture: true,
  types: "zip rar 7z tar gz tgz bz2 xz zst iso img dmg exe msi apk deb rpm mp4 mkv avi mov webm m4v mp3 flac m4a wav ogg opus pdf epub",
  minSizeMB: 1,
  exclude: "",
};

async function settings() {
  return { ...DEFAULTS, ...(await api.storage.local.get(DEFAULTS)) };
}

// send hands links to Siphon and returns how many it took. With page set they
// were collected from a whole page, and Siphon keeps only the supported ones.
async function send(links, { page = false, referrer = "" } = {}) {
  let reply;
  try {
    reply = await api.runtime.sendNativeMessage(HOST, { type: "add", links, page, referrer });
  } catch (e) {
    throw new Error("Siphon can't be reached: install the browser integration in Siphon > Settings. (" + ((e && e.message) || e) + ")");
  }
  if (!reply || !reply.ok) {
    throw new Error((reply && reply.error) || "Siphon didn't answer");
  }
  return reply.added || 0;
}

// The badge on the toolbar button says what happened, for a few seconds.
let badgeTimer;
function show(text, title, color) {
  api.action.setBadgeText({ text });
  api.action.setBadgeBackgroundColor({ color: color || "#2563eb" });
  api.action.setTitle({ title: title || TITLE });
  clearTimeout(badgeTimer);
  badgeTimer = setTimeout(() => {
    api.action.setBadgeText({ text: "" });
    api.action.setTitle({ title: TITLE });
  }, 6000);
}

function report(promise) {
  return promise.then(
    (n) => show(String(n), n > 0 ? `Siphon took ${n} link(s)` : "No file links of a supported site here"),
    (e) => show("!", e.message, "#dc2626"),
  );
}

// Text after a link in a sentence isn't part of it.
const trimLink = (l) => l.replace(/[.,;:!?'")\]]+$/, "");

// pageLinks collects every link of the tab's page, the page itself included.
async function pageLinks(tab) {
  const [res] = await api.scripting.executeScript({
    target: { tabId: tab.id },
    func: () => [...new Set([location.href, ...[...document.querySelectorAll("a[href]")].map((a) => a.href)])],
  });
  return (res && res.result) || [];
}

// selectedLinks collects the links in the selection: its anchors and the web
// addresses written in its text.
async function selectedLinks(tab) {
  const [res] = await api.scripting.executeScript({
    target: { tabId: tab.id },
    func: () => {
      const sel = getSelection();
      const out = new Set();
      for (const a of document.querySelectorAll("a[href]")) {
        if (sel.containsNode(a, true)) out.add(a.href);
      }
      for (const m of sel.toString().matchAll(/https?:\/\/[^\s<>"']+/g)) out.add(m[0]);
      return [...out];
    },
  });
  return ((res && res.result) || []).map(trimLink);
}

const MENUS = [
  { id: "siphon-link", title: "Download with Siphon", contexts: ["link"] },
  { id: "siphon-selection", title: "Download the selected links with Siphon", contexts: ["selection"] },
  { id: "siphon-page", title: TITLE, contexts: ["page"] },
];

async function createMenus() {
  try {
    await api.contextMenus.removeAll();
  } catch (e) {
    // nothing to remove
  }
  for (const m of MENUS) {
    api.contextMenus.create(m, () => void api.runtime.lastError);
  }
}
api.runtime.onInstalled.addListener(createMenus);
if (api.runtime.onStartup) api.runtime.onStartup.addListener(createMenus);

api.contextMenus.onClicked.addListener((info, tab) => {
  switch (info.menuItemId) {
    case "siphon-link":
      // A link the user chose goes as it is, whatever the site.
      report(send([info.linkUrl], { referrer: info.pageUrl || "" }));
      break;
    case "siphon-selection":
      report(selectedLinks(tab).then((links) => send(links, { referrer: info.pageUrl || "" })));
      break;
    case "siphon-page":
      report(pageLinks(tab).then((links) => send(links, { page: true, referrer: tab.url || "" })));
      break;
  }
});

api.action.onClicked.addListener((tab) => {
  report(pageLinks(tab).then((links) => send(links, { page: true, referrer: tab.url || "" })));
});

// --- Download capture ---
//
// A download the browser starts goes to Siphon instead when it matches the
// rules: a file type in the list, not smaller than the size, not from an
// excluded site. The download is paused first and canceled only once Siphon
// took the link; if Siphon can't be reached it goes on in the browser.
// Siphon fetches the link itself, without the browser's cookies: exclude a
// site whose downloads need you to be signed in.

function extOf(name) {
  const m = /\.([a-z0-9]{1,8})$/i.exec(name || "");
  return m ? m[1].toLowerCase() : "";
}

function excluded(url, list) {
  let host = "";
  try {
    host = new URL(url).hostname.toLowerCase();
  } catch (e) {
    return true;
  }
  return list
    .toLowerCase()
    .split(/[\s,]+/)
    .filter(Boolean)
    .some((d) => host === d || host.endsWith("." + d));
}

function wanted(item, url, s) {
  const size = item.totalBytes > 0 ? item.totalBytes : item.fileSize > 0 ? item.fileSize : -1;
  if (size >= 0 && size < s.minSizeMB * 1024 * 1024) return false;
  let path = "";
  try {
    path = decodeURIComponent(new URL(url).pathname);
  } catch (e) {
    // keep "" : the file name may still say
  }
  const types = new Set(s.types.toLowerCase().split(/[\s,]+/).filter(Boolean));
  const ext = extOf((item.filename || "").split(/[\\/]/).pop()) || extOf(path);
  return ext !== "" && types.has(ext);
}

api.downloads.onCreated.addListener(async (item) => {
  const s = await settings();
  const url = item.finalUrl || item.url;
  if (!s.capture || !/^https?:\/\//i.test(url) || excluded(url, s.exclude) || !wanted(item, url, s)) return;
  try {
    await api.downloads.pause(item.id);
  } catch (e) {
    // it may not have started yet; canceling below still stops it
  }
  try {
    await send([url], { referrer: item.referrer || "" });
  } catch (e) {
    api.downloads.resume(item.id).catch(() => {});
    show("!", e.message + " The browser keeps the download.", "#dc2626");
    return;
  }
  await api.downloads.cancel(item.id).catch(() => {});
  await api.downloads.erase({ id: item.id }).catch(() => {});
  show("✓", "The download went to Siphon");
});
