// Injected toolbar for the chrome-less Sybra in-app browser window.
// Throwaway shim pending a native NSToolbar (Wails v3 exposes no native
// back/forward/URL API yet) — see main.go's openInAppBrowser.
(function () {
  try {
    var HOST_ID = "sybra-nav-host";
    var TOOLBAR_HEIGHT = 40;

    function sync(input) {
      input.value = location.href;
    }

    var existingHost = document.getElementById(HOST_ID);
    if (existingHost) {
      var existingInput = existingHost.shadowRoot && existingHost.shadowRoot.getElementById("address");
      if (existingInput) sync(existingInput);
      return;
    }

    if (!document.body) return;

    function normalizeURL(raw) {
      var value = (raw || "").trim();
      if (!value) return null;

      var schemeMatch = /^[a-z][a-z0-9+.-]*:/i.exec(value);
      if (schemeMatch) {
        var scheme = schemeMatch[0].toLowerCase();
        if (scheme === "http:" || scheme === "https:") return value;
        return null;
      }

      if (value.charAt(0) === "#") return null;
      return "https://" + value;
    }

    var host = document.createElement("div");
    host.id = HOST_ID;
    host.style.position = "fixed";
    host.style.top = "0";
    host.style.left = "0";
    host.style.right = "0";
    host.style.zIndex = "2147483647";
    document.documentElement.appendChild(host);

    var shadow = host.attachShadow({ mode: "open" });

    var style = document.createElement("style");
    style.textContent =
      ":host { all: initial; }" +
      ".toolbar { display: flex; align-items: center; gap: 6px; height: " +
      TOOLBAR_HEIGHT +
      "px; padding: 0 8px; box-sizing: border-box; background: #2d2d2d; font-family: -apple-system, BlinkMacSystemFont, sans-serif; }" +
      "button { border: none; border-radius: 4px; background: #444; color: #eee; width: 28px; height: 28px; font-size: 14px; cursor: pointer; }" +
      "button:hover { background: #555; }" +
      "input { flex: 1; height: 26px; border-radius: 4px; border: 1px solid #555; background: #1e1e1e; color: #eee; padding: 0 8px; font-size: 13px; }";
    shadow.appendChild(style);

    var toolbar = document.createElement("div");
    toolbar.className = "toolbar";
    toolbar.innerHTML =
      '<button id="back" title="Back">◀</button>' +
      '<button id="forward" title="Forward">▶</button>' +
      '<button id="reload" title="Reload">⟳</button>' +
      '<input id="address" type="text" spellcheck="false" />';
    shadow.appendChild(toolbar);

    document.documentElement.style.setProperty("padding-top", TOOLBAR_HEIGHT + "px", "important");

    var addressInput = shadow.getElementById("address");
    addressInput.addEventListener("keydown", function (event) {
      if (event.key !== "Enter") return;
      var href = normalizeURL(addressInput.value);
      if (href) location.assign(href);
    });

    shadow.getElementById("back").addEventListener("click", function () {
      history.back();
    });
    shadow.getElementById("forward").addEventListener("click", function () {
      history.forward();
    });
    shadow.getElementById("reload").addEventListener("click", function () {
      location.reload();
    });

    if (!history.__sybraPatched) {
      history.__sybraPatched = true;
      ["pushState", "replaceState"].forEach(function (method) {
        var original = history[method];
        history[method] = function () {
          var result = original.apply(history, arguments);
          sync(addressInput);
          return result;
        };
      });
      window.addEventListener("popstate", function () {
        sync(addressInput);
      });
      window.addEventListener("hashchange", function () {
        sync(addressInput);
      });
    }

    sync(addressInput);
  } catch {
    // Swallow — a broken toolbar must never break the underlying page.
  }
})();
