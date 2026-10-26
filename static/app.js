// The pages' islands (no build step): every page works without this file;
// it adds the console's live log and commands, the uploads that hash and
// resume, the bulk selection, the copy buttons, and a fallback when the live
// stream fails. Served from /static (the Content-Security-Policy allows no
// inline script).
"use strict";

(function () {
  const csrf = () => {
    const input = document.querySelector('input[name="csrf"]');
    return input ? input.value : "";
  };

  // ---------- copy buttons
  function copies(root) {
    root.querySelectorAll("[data-copy]").forEach((button) => {
      button.hidden = false;
      button.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(button.dataset.copy);
          const text = button.lastChild;
          const before = text.textContent;
          text.textContent = " Copied";
          setTimeout(() => (text.textContent = before), 1500);
        } catch (e) {
          /* (the value stays selectable) */
        }
      });
    });
  }

  // ---------- the bulk bar: the selection's count, and the fields of the
  // action chosen
  function bulk() {
    const all = document.querySelector("[data-select-all]");
    const count = document.querySelector("[data-selected]");
    const boxes = () => Array.from(document.querySelectorAll("[data-select]"));
    const update = () => {
      const n = boxes().filter((b) => b.checked).length;
      if (count) count.textContent = n ? n + " selected" : "select";
    };
    if (all) {
      all.addEventListener("change", () => {
        boxes().forEach((b) => (b.checked = all.checked));
        update();
      });
    }
    document.addEventListener("change", (e) => {
      if (e.target.matches && e.target.matches("[data-select]")) update();
    });
    const action = document.querySelector("[data-bulk-action]");
    if (action) {
      const show = () => {
        document.querySelectorAll("#bulk [data-for]").forEach((field) => {
          field.hidden = !field.dataset.for.split(" ").includes(action.value);
        });
      };
      action.addEventListener("change", show);
      show();
    }
  }

  // ---------- the console: the log live over a WebSocket, the commands
  function console_() {
    const root = document.querySelector("[data-console]");
    if (!root) return;
    const slug = root.dataset.console;
    const log = root.querySelector("[data-console-log]");
    const form = root.querySelector("[data-console-form]");
    const follow = root.querySelector("[data-follow]");
    const hidden = new Set();
    root.querySelectorAll("[data-console-chips] input[value]").forEach((chip) => {
      chip.addEventListener("change", () => {
        if (chip.checked) hidden.delete(chip.value);
        else hidden.add(chip.value);
        log.querySelectorAll("[data-cat]").forEach((line) => {
          line.hidden = hidden.has(line.dataset.cat);
        });
      });
    });
    const clock = (ms) => {
      const d = new Date(ms);
      return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
    };
    const add = (line, extra) => {
      const div = document.createElement("div");
      div.dataset.cat = line.cat || "console";
      if (line.level === "error") div.className = "text-bad";
      else if (line.level === "warn") div.className = "text-warn";
      if (extra) div.className = extra;
      const time = document.createElement("span");
      time.className = "text-faint";
      time.textContent = (line.time ? clock(line.time) : "") + " " + (line.cat || "") + " ";
      div.append(time, document.createTextNode(line.text));
      div.hidden = hidden.has(div.dataset.cat);
      log.append(div);
      while (log.childElementCount > 4000) log.firstElementChild.remove();
      if (!follow || follow.checked) log.scrollTop = log.scrollHeight;
    };
    log.scrollTop = log.scrollHeight;
    let socket;
    let wait = 1000;
    const connect = () => {
      const scheme = location.protocol === "https:" ? "wss://" : "ws://";
      socket = new WebSocket(scheme + location.host + "/ws/console/" + encodeURIComponent(slug));
      socket.addEventListener("open", () => (wait = 1000));
      socket.addEventListener("message", (e) => {
        const m = JSON.parse(e.data);
        if (m.type === "lines") {
          log.textContent = "";
          m.lines.forEach((l) => add(l));
        } else if (m.type === "line") {
          add(m.line);
        } else if (m.type === "reply") {
          add({ cat: "console", text: "› " + m.line }, "text-accent");
          (m.lines || []).forEach((t) => add({ cat: "console", text: t, level: m.ok ? "" : "warn" }));
        } else if (m.type === "help" && form) {
          let list = document.getElementById("console-help");
          if (!list) {
            list = document.createElement("datalist");
            list.id = "console-help";
            form.append(list);
          }
          list.textContent = "";
          new Set(m.commands.filter((w) => /^[a-z_]+$/.test(w))).forEach((w) => {
            const option = document.createElement("option");
            option.value = w;
            list.append(option);
          });
        }
      });
      socket.addEventListener("close", () => {
        setTimeout(connect, wait);
        wait = Math.min(wait * 2, 30000);
      });
    };
    connect();
    if (form) {
      const input = form.querySelector('input[name="line"]');
      const history = [];
      let at = 0;
      form.addEventListener("submit", (e) => {
        if (!socket || socket.readyState !== WebSocket.OPEN) return;
        e.preventDefault();
        const line = input.value.trim();
        if (!line) return;
        socket.send(JSON.stringify({ line }));
        history.push(line);
        at = history.length;
        input.value = "";
      });
      input.addEventListener("keydown", (e) => {
        if (e.key === "ArrowUp" && at > 0) {
          at--;
          input.value = history[at];
          e.preventDefault();
        } else if (e.key === "ArrowDown" && at < history.length) {
          at++;
          input.value = history[at] || "";
          e.preventDefault();
        }
      });
    }
  }

  // ---------- uploads: hashed in the browser (a retry of a file the library
  // has is done at once), sent in chunks that resume where they stopped
  function uploads() {
    // (a folder's file goes by the folder it is in, as library.Classify does
    // on the server; a file picked alone by the kind chosen)
    const resources = ["bitmaps", "sounds", "loc"];
    const extensions = { map: ".map", custom_map: ".map", resource: ".map", gametype: ".toml", saved_gametype: ".gametype" };
    const classify = (file, chosen) => {
      const path = (file.webkitRelativePath || "").toLowerCase();
      const base = file.name.toLowerCase();
      if (path) {
        const parts = path.split("/");
        const dir = parts.length > 1 ? parts[parts.length - 2] : "";
        const stem = base.replace(/\.[^.]*$/, "");
        if (dir === "maps" && base.endsWith(".map")) return { kind: "map", name: stem };
        if (dir === "custom_maps" && base.endsWith(".map"))
          return { kind: resources.includes(stem) ? "resource" : "custom_map", name: stem };
        if (dir === "gametypes" && base.endsWith(".toml")) return { kind: "gametype", name: stem };
        if (dir === "gametypes" && base.endsWith(".gametype")) return { kind: "saved_gametype", name: stem };
        return null;
      }
      if (!base.endsWith(extensions[chosen])) return null;
      return { kind: chosen, name: base.slice(0, -extensions[chosen].length) };
    };

    document.querySelectorAll("[data-upload]").forEach((form) => {
      if (!window.crypto || !crypto.subtle) return;
      const bar = form.querySelector("[data-upload-bar]");
      const fill = form.querySelector("[data-upload-fill]");
      const words = form.querySelector("[data-upload-words]");
      const say = (text) => (words.textContent = text);
      const progress = (done, total) => {
        bar.classList.remove("hidden");
        fill.style.width = total ? ((done / total) * 100).toFixed(1) + "%" : "0";
      };
      const headers = () => ({ "Content-Type": "application/json", "X-CSRF-Token": csrf(), Accept: "application/json" });

      // send uploads one file, resumable: "added" or "had" (the library had it)
      const send = async (item, label) => {
        const { file, kind, name } = item;
        say(label + ": hashing…");
        progress(0, file.size);
        const digest = await crypto.subtle.digest("SHA-256", await file.arrayBuffer());
        const sha = Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
        let r = await fetch("/library/uploads", {
          method: "POST", headers: headers(), body: JSON.stringify({ kind, name, size: file.size, sha256: sha }),
        });
        let answer = await r.json();
        if (!answer.ok) throw new Error(answer.error.message);
        if (answer.done) return "had";
        const id = answer.id;
        const chunk = 8 << 20;
        let offset = 0;
        while (offset < file.size) {
          r = await fetch("/library/uploads/" + id + "?offset=" + offset, {
            method: "PUT", headers: { "X-CSRF-Token": csrf(), Accept: "application/json" },
            body: file.slice(offset, offset + chunk),
          });
          answer = await r.json();
          if (r.status === 409 && answer.received !== undefined) {
            offset = answer.received;
            continue;
          }
          if (!answer.ok) throw new Error(answer.error.message);
          offset = answer.received;
          progress(offset, file.size);
          say(label + ": sent " + (offset >> 20) + " of " + (file.size >> 20) + " MB");
        }
        r = await fetch("/library/uploads/" + id + "/finish", { method: "POST", headers: headers() });
        answer = await r.json();
        if (!answer.ok) throw new Error(answer.error.message);
        return "added";
      };

      form.addEventListener("submit", async (e) => {
        const picked = [...form.querySelectorAll('input[type="file"]')].flatMap((input) => [...input.files]);
        e.preventDefault();
        if (!picked.length) {
          say("Choose files, or a folder.");
          return;
        }
        const chosen = form.querySelector('[name="kind"]').value;
        const items = [];
        const skipped = [];
        for (const file of picked) {
          const item = classify(file, chosen);
          if (item) items.push({ file, ...item });
          else skipped.push(file.webkitRelativePath || file.name);
        }
        const typed = form.querySelector('[name="name"]').value.trim();
        if (typed && items.length === 1) items[0].name = typed;
        if (!items.length) {
          say("Nothing to add: " + (skipped.length === 1 ? skipped[0] + " is" : "these are") +
            " not of the kind chosen, or not in a maps/, custom_maps/ or gametypes/ folder.");
          return;
        }
        const button = form.querySelector('button[type="submit"]');
        button.disabled = true;
        let added = 0, had = 0;
        const failed = [];
        for (let n = 0; n < items.length; n++) {
          const label = items[n].file.name + (items.length > 1 ? " (" + (n + 1) + " of " + items.length + ")" : "");
          try {
            if ((await send(items[n], label)) === "added") added++;
            else had++;
          } catch (err) {
            failed.push(items[n].file.name + ": " + err.message);
          }
        }
        button.disabled = false;
        const parts = [];
        if (added) parts.push(added + " added");
        if (had) parts.push(had + " the library had already");
        if (skipped.length) parts.push(skipped.length + " skipped (not of the kind, or in no maps/, custom_maps/ or gametypes/ folder)");
        let text = (parts.join(", ") || "Nothing added") + ".";
        if (failed.length) text += " Not sent: " + failed.join("; ") + " (send them again: they resume).";
        say(text);
        if (!failed.length && (added || had)) setTimeout(() => location.reload(), 1500);
      });
    });
  }

  // ---------- the live stream: a resync reloads; a stream that fails falls
  // back to reloading the page now and then
  function stream() {
    let failures = 0;
    let timer = null;
    document.body.addEventListener("htmx:sseError", () => {
      failures++;
      if (failures >= 2 && !timer) {
        timer = setInterval(() => {
          if (document.visibilityState === "visible" && !document.querySelector("details[open], input:focus, textarea:focus")) {
            location.reload();
          }
        }, 10000);
      }
    });
    document.body.addEventListener("htmx:sseOpen", () => {
      failures = 0;
      if (timer) {
        clearInterval(timer);
        timer = null;
      }
    });
    document.body.addEventListener("htmx:sseMessage", (e) => {
      if (e.detail && e.detail.type === "resync") location.reload();
    });
    document.body.addEventListener("htmx:afterSwap", (e) => copies(e.target));
  }

  // ---------- an open sheet closes with Escape or a click elsewhere
  function sheets() {
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") document.querySelectorAll("details[open]").forEach((d) => d.removeAttribute("open"));
    });
    document.addEventListener("click", (e) => {
      document.querySelectorAll("details[open]").forEach((d) => {
        if (!d.contains(e.target)) d.removeAttribute("open");
      });
    });
  }

  // ---------- the theme switch, when the page follows the system's scheme:
  // it offers the other one and tells the server which is shown
  function theme() {
    if (document.documentElement.dataset.theme) return;
    if (!window.matchMedia("(prefers-color-scheme: light)").matches) return;
    document.querySelectorAll("[data-theme-current]").forEach((input) => (input.value = "light"));
    document.querySelectorAll("[data-theme-toggle]").forEach((button) => {
      const use = button.querySelector("use");
      if (use) use.setAttribute("href", "/static/icons.svg#i-moon");
      const label = [...button.childNodes].find((n) => n.nodeType === Node.TEXT_NODE && n.textContent.trim());
      if (label) label.textContent = "dark";
    });
  }

  // ---------- the live stream: what changed before it opened (or while it
  // was down) is asked again; it is closed before the page goes, so that
  // its end is no error
  function live() {
    document.body.addEventListener("htmx:sseOpen", () => {
      document.querySelectorAll("[data-refresh]").forEach((el) => {
        htmx.ajax("GET", el.dataset.refresh, { target: el, swap: "outerHTML" });
      });
    });
    const close = () => {
      const data = document.body["htmx-internal-data"];
      if (data && data.sseEventSource) data.sseEventSource.close();
    };
    window.addEventListener("beforeunload", close);
    window.addEventListener("pagehide", close);
  }

  document.addEventListener("DOMContentLoaded", () => {
    theme();
    live();
    copies(document);
    bulk();
    console_();
    uploads();
    stream();
    sheets();
  });
})();
