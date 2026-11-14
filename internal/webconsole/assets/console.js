"use strict";

const status = document.querySelector("[data-status-url]");
if (status) {
  let tag = "";
  let pending = false;
  const refresh = async () => {
    if (document.hidden || pending) return;
    pending = true;
    try {
      const options = { cache: "no-cache", credentials: "same-origin" };
      if (tag) options.headers = { "If-None-Match": tag };
      const response = await fetch(status.dataset.statusUrl, options);
      if (response.status !== 200) return;
      const fragment = new DOMParser().parseFromString(await response.text(), "text/html");
      const mode = fragment.body.querySelector(".chip");
      if (mode && mode.textContent !== status.textContent.trim()) {
        const label = document.createElement("span");
        label.className = mode.classList.contains("hold") ? "chip hold" : "chip";
        label.textContent = mode.textContent;
        status.replaceChildren(label);
      }
      tag = response.headers.get("ETag") || "";
    } catch {
      // Keep the last rendered local state when a refresh fails.
    } finally {
      pending = false;
    }
  };
  window.setInterval(refresh, 20000);
  document.addEventListener("visibilitychange", refresh);
}

for (const form of document.querySelectorAll("form[data-approval]")) {
  const button = form.querySelector("button[type=submit]");
  if (button) {
    form.addEventListener("submit", () => { button.disabled = true; });
    window.addEventListener("pageshow", () => { button.disabled = false; });
  }
}
