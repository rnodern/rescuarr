"use strict";

const timestampFormat = new Intl.DateTimeFormat(undefined, {
  year: "numeric", month: "short", day: "numeric", hour: "numeric",
  minute: "2-digit", timeZoneName: "short",
});
for (const time of document.querySelectorAll("time[data-local-time]")) {
  const date = new Date(time.dateTime);
  if (!Number.isNaN(date.getTime())) time.textContent = timestampFormat.format(date);
}

const status = document.querySelector("[data-status-url]");
const spinner = document.getElementById("episode-scanning");
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
      if (response.status === 304) return;
      if (!response.ok) {
        if (spinner) spinner.hidden = true;
        tag = "";
        return;
      }
      const fragment = new DOMParser().parseFromString(await response.text(), "text/html");
      const next = fragment.querySelector(".chip");
      const mode = status.querySelector(".chip");
      if (next && mode) {
        if (next.textContent !== mode.textContent) mode.textContent = next.textContent;
        mode.classList.toggle("hold", next.classList.contains("hold"));
        if (spinner) spinner.hidden = next.dataset.episodeScanning !== "true";
      }
      tag = response.headers.get("ETag") || "";
    } catch {
      // Keep the last rendered mode when a refresh fails.
      if (spinner) spinner.hidden = true;
      tag = "";
    } finally {
      pending = false;
    }
  };
  refresh();
  window.setInterval(refresh, 1500);
  document.addEventListener("visibilitychange", refresh);
}

const filters = document.querySelector("form[data-queue-filters]");
const results = document.querySelector("[data-queue-results]");
if (filters && results) {
  let timer;
  let request;
  const update = async () => {
    const current = new AbortController();
    request = current;
    const url = new URL(filters.action);
    for (const [key, value] of new FormData(filters)) {
      if (value) url.searchParams.set(key, value);
    }
    results.setAttribute("aria-busy", "true");
    try {
      const response = await fetch(url, { signal: current.signal, credentials: "same-origin" });
      if (!response.ok || response.redirected) throw new Error("Queue refresh failed");
      const fragment = new DOMParser().parseFromString(await response.text(), "text/html");
      const updated = fragment.querySelector("[data-queue-results]");
      if (!updated) throw new Error("Missing queue results");
      if (current.signal.aborted) return;
      results.replaceChildren(...updated.childNodes);
      window.history.replaceState(null, "", url);
    } catch {
      if (!current.signal.aborted) window.location.assign(url);
    } finally {
      if (request === current) results.setAttribute("aria-busy", "false");
    }
  };
  const schedule = (delay) => {
    window.clearTimeout(timer);
    request?.abort();
    timer = window.setTimeout(update, delay);
  };
  filters.addEventListener("input", (event) => {
    if (event.target.matches("input[type=search]")) schedule(200);
  });
  filters.addEventListener("change", (event) => {
    if (event.target.matches("select, input[type=checkbox]")) schedule(0);
  });
  filters.addEventListener("submit", (event) => {
    event.preventDefault();
    schedule(0);
  });
}

for (const form of document.querySelectorAll("form[data-approval]")) {
  const button = form.querySelector("button[type=submit]");
  if (button) {
    form.addEventListener("submit", () => { button.disabled = true; });
    window.addEventListener("pageshow", () => { button.disabled = false; });
  }
}
