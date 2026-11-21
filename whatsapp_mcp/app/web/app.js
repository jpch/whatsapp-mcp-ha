"use strict";
const $ = (id) => document.getElementById(id);
const labels = {
  starting: ["Starting", "warn"], waiting_for_qr: ["Waiting for QR scan", "warn"],
  pairing_timeout: ["Not linked (QR expired)", "bad"], connecting: ["Connecting", "warn"],
  connected: ["Connected", "ok"], disconnected: ["Disconnected, reconnecting", "warn"],
  logged_out: ["Unlinked", "bad"],
};
function post(path) {
  return fetch(path, { method: "POST", headers: { "X-Requested-With": "whatsapp-mcp" } });
}
let lastQR = 0;
async function refresh() {
  try {
    const r = await fetch("api/status", { cache: "no-store" });
    const s = await r.json();
    const wa = s.whatsapp;
    const [label, cls] = labels[wa.state] || [wa.state, "warn"];
    $("state").textContent = label;
    $("state").className = "state " + cls;
    $("account").textContent = wa.paired && wa.account ? "(" + wa.account + ")" : "";
    $("pairing").classList.toggle("hidden", !s.has_qr);
    if (s.has_qr && Date.now() - lastQR > 5000) {
      $("qr").src = "api/qr.png?t=" + Date.now();
      lastQR = Date.now();
    }
    $("newqr").classList.toggle("hidden", !(wa.state === "pairing_timeout" || wa.state === "logged_out"));
    $("unlink").classList.toggle("hidden", !wa.paired);
    $("url").textContent = s.internal_url;
    $("send").textContent = s.send_enabled ? "enabled" : "disabled";
    $("download").textContent = s.media_download ? "enabled" : "disabled";
    $("counts").textContent = s.chats + " chats, " + s.messages + " messages";
    $("version").textContent = s.version;
    $("tokenbox").classList.toggle("hidden", !s.token_generated);
    $("showtoken").classList.toggle("hidden", !!s.token_shown);
    $("shownote").classList.toggle("hidden", !s.token_shown);
  } catch (e) {
    $("state").textContent = "add-on not reachable";
    $("state").className = "state bad";
  }
}
$("pair").addEventListener("click", async () => { await post("api/pair"); refresh(); });
$("logout").addEventListener("click", async () => {
  if (!confirm("Unlink this device from WhatsApp? You will need to scan a new QR code.")) return;
  const r = await post("api/logout");
  if (!r.ok) alert("Could not unlink: " + ((await r.json()).error || r.status));
  refresh();
});
function showToken(j) {
  if (j.token) { $("token").textContent = j.token; $("token").classList.remove("hidden"); }
  refresh();
}
$("showtoken").addEventListener("click", async () => {
  showToken(await (await post("api/token")).json());
});
$("rotate").addEventListener("click", async () => {
  if (!confirm("Generate a new token? MCP clients using the current token will stop working.")) return;
  showToken(await (await post("api/token/rotate")).json());
});
refresh();
setInterval(refresh, 3000);
